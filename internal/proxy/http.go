package proxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"surfshark-proxy/internal/config"
	"surfshark-proxy/internal/parser"
	"surfshark-proxy/internal/router"
)

// HTTPProxyServer 提供 HTTP/HTTPS 代理入口。
type HTTPProxyServer struct {
	cfg      config.Config
	router   *router.Router
	resolver NsHandleResolver
	dialer   *NsDialer
}

// NewHTTPProxyServer 创建 HTTP 代理服务器。
func NewHTTPProxyServer(cfg config.Config, router *router.Router, resolver NsHandleResolver) *HTTPProxyServer {
	return &HTTPProxyServer{
		cfg:      cfg,
		router:   router,
		resolver: resolver,
		dialer:   &NsDialer{},
	}
}

// ListenAndServe 启动 HTTP 代理服务器。
func (s *HTTPProxyServer) ListenAndServe(ctx context.Context) error {
	address := fmt.Sprintf(":%d", s.cfg.HTTPPort)
	server := &http.Server{
		Addr:    address,
		Handler: s,
	}

	log.Printf("HTTP 代理已监听 %s", address)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	err := server.ListenAndServe()
	if err != nil && err != http.ErrServerClosed && ctx.Err() == nil {
		return err
	}

	return nil
}

// ServeHTTP 处理 HTTP CONNECT 与普通 HTTP 代理请求。
func (s *HTTPProxyServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	username, password, ok := s.parseProxyAuth(request)
	if !ok {
		s.writeProxyAuthRequired(writer)
		return
	}

	params := parser.Parse(username, password)
	if params.Username != s.cfg.ProxyUser || password != s.cfg.ProxyPass {
		s.writeProxyAuthRequired(writer)
		return
	}

	if request.Method == http.MethodConnect {
		s.handleConnect(writer, request, params)
		return
	}

	s.handleHTTP(writer, request, params)
}

func (s *HTTPProxyServer) writeProxyAuthRequired(writer http.ResponseWriter) {
	writer.Header().Set("Proxy-Authenticate", `Basic realm="proxy"`)
	http.Error(writer, "需要代理认证", http.StatusProxyAuthRequired)
}

func (s *HTTPProxyServer) handleConnect(writer http.ResponseWriter, request *http.Request, params parser.Params) {
	targetConn, err := s.dialTarget(request.Context(), params, request.Host)
	if err != nil {
		http.Error(writer, fmt.Sprintf("连接目标失败: %v", err), http.StatusBadGateway)
		return
	}
	defer targetConn.Close()

	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		http.Error(writer, "当前响应不支持 hijack", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(writer, fmt.Sprintf("hijack 失败: %v", err), http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}

	s.pipe(clientConn, targetConn)
}

func (s *HTTPProxyServer) handleHTTP(writer http.ResponseWriter, request *http.Request, params parser.Params) {
	targetAddress := request.URL.Host
	if targetAddress == "" {
		targetAddress = request.Host
	}
	if !strings.Contains(targetAddress, ":") {
		targetAddress += ":80"
	}

	targetConn, err := s.dialTarget(request.Context(), params, targetAddress)
	if err != nil {
		http.Error(writer, fmt.Sprintf("连接目标失败: %v", err), http.StatusBadGateway)
		return
	}
	defer targetConn.Close()

	request.Header.Del("Proxy-Authorization")
	request.Header.Del("Proxy-Connection")
	request.RequestURI = ""

	if err := request.Write(targetConn); err != nil {
		http.Error(writer, fmt.Sprintf("转发请求失败: %v", err), http.StatusBadGateway)
		return
	}

	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		http.Error(writer, "当前响应不支持 hijack", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer clientConn.Close()

	_, _ = io.Copy(clientConn, targetConn)
}

func (s *HTTPProxyServer) dialTarget(ctx context.Context, params parser.Params, address string) (net.Conn, error) {
	worker, err := s.router.Route(params)
	if err != nil {
		return nil, fmt.Errorf("route http request: %w", err)
	}

	nsHandle, err := s.resolver.GetWorkerNsHandle(worker.ID)
	if err != nil {
		return nil, fmt.Errorf("resolve worker namespace: %w", err)
	}

	s.resolver.TrackConn(worker.ID)
	conn, err := s.dialer.DialInNs(ctx, nsHandle, "tcp", address)
	if err != nil {
		s.resolver.UntrackConn(worker.ID)
		return nil, err
	}

	return &trackedConn{Conn: conn, workerID: worker.ID, resolver: s.resolver}, nil
}

func (s *HTTPProxyServer) parseProxyAuth(request *http.Request) (string, string, bool) {
	header := request.Header.Get("Proxy-Authorization")
	if !strings.HasPrefix(header, "Basic ") {
		return "", "", false
	}

	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, "Basic "))
	if err != nil {
		return "", "", false
	}

	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return "", "", false
	}

	return parts[0], parts[1], true
}

func (s *HTTPProxyServer) pipe(left, right net.Conn) {
	done := make(chan struct{}, 2)

	copyConn := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if closer, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = closer.CloseWrite()
		}
		done <- struct{}{}
	}

	go copyConn(left, right)
	go copyConn(right, left)

	<-done
	<-done
}
