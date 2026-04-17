package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync/atomic"

	socks5 "github.com/things-go/go-socks5"
	vishnetns "github.com/vishvananda/netns"
	"surfshark-proxy/internal/config"
	"surfshark-proxy/internal/parser"
	"surfshark-proxy/internal/router"
)

// NsHandleResolver 提供代理层访问 worker 命名空间的能力。
type NsHandleResolver interface {
	GetWorkerNsHandle(workerID string) (vishnetns.NsHandle, error)
	TrackConn(workerID string)
	UntrackConn(workerID string)
}

// Socks5Server 提供 SOCKS5 代理入口。
type Socks5Server struct {
	cfg      config.Config
	router   *router.Router
	resolver NsHandleResolver
	dialer   *NsDialer
}

// NewSocks5Server 创建 SOCKS5 代理服务器。
func NewSocks5Server(cfg config.Config, router *router.Router, resolver NsHandleResolver) *Socks5Server {
	return &Socks5Server{
		cfg:      cfg,
		router:   router,
		resolver: resolver,
		dialer:   &NsDialer{},
	}
}

type credentialStore struct {
	cfg config.Config
}

func (store *credentialStore) Valid(user, password, userAddr string) bool {
	_ = userAddr
	params := parser.Parse(user, password)
	return params.Username == store.cfg.ProxyUser && password == store.cfg.ProxyPass
}

// ListenAndServe 启动 SOCKS5 服务。
func (s *Socks5Server) ListenAndServe(ctx context.Context) error {
	server := socks5.NewServer(
		socks5.WithAuthMethods([]socks5.Authenticator{
			socks5.UserPassAuthenticator{
				Credentials: &credentialStore{cfg: s.cfg},
			},
		}),
		socks5.WithDialAndRequest(s.dialWithRequest),
	)

	address := fmt.Sprintf(":%d", s.cfg.Socks5Port)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen socks5 on %s: %w", address, err)
	}

	log.Printf("SOCKS5 代理已监听 %s", address)

	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	err = server.Serve(listener)
	if err != nil && !errors.Is(err, net.ErrClosed) && ctx.Err() == nil {
		return err
	}

	return nil
}

func (s *Socks5Server) dialWithRequest(ctx context.Context, network, address string, request *socks5.Request) (net.Conn, error) {
	username := ""
	if request != nil && request.AuthContext != nil && request.AuthContext.Payload != nil {
		username = request.AuthContext.Payload["username"]
	}

	params := parser.Parse(username, "")
	worker, err := s.router.Route(params)
	if err != nil {
		return nil, fmt.Errorf("route socks5 request: %w", err)
	}

	nsHandle, err := s.resolver.GetWorkerNsHandle(worker.ID)
	if err != nil {
		return nil, fmt.Errorf("resolve worker namespace: %w", err)
	}

	s.resolver.TrackConn(worker.ID)
	conn, err := s.dialer.DialInNs(ctx, nsHandle, network, address)
	if err != nil {
		s.resolver.UntrackConn(worker.ID)
		return nil, err
	}

	return &trackedConn{Conn: conn, workerID: worker.ID, resolver: s.resolver}, nil
}

type trackedConn struct {
	net.Conn
	workerID string
	resolver NsHandleResolver
	closed   atomic.Bool
}

func (conn *trackedConn) Close() error {
	if conn.closed.CompareAndSwap(false, true) {
		conn.resolver.UntrackConn(conn.workerID)
	}
	return conn.Conn.Close()
}

func (conn *trackedConn) Read(buffer []byte) (int, error) {
	n, err := conn.Conn.Read(buffer)
	if errors.Is(err, io.EOF) {
		_ = conn.Close()
	}
	return n, err
}
