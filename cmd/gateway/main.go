package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"surfshark-proxy/internal/config"
	"surfshark-proxy/internal/discovery"
	"surfshark-proxy/internal/proxy"
	"surfshark-proxy/internal/router"
	"surfshark-proxy/internal/session"
	workermgr "surfshark-proxy/internal/worker"
)

func main() {
	cfg := config.Load()

	enableIPForwarding()

	servers, err := discovery.Scan(cfg.OvpnDir)
	if err != nil {
		log.Fatalf("扫描 OVPN 目录失败: %v", err)
	}

	totalServers := 0
	for _, group := range servers {
		totalServers += len(group)
	}
	if totalServers == 0 {
		log.Fatalf("未在 %s 中发现可用的 .ovpn 文件", cfg.OvpnDir)
	}

	log.Printf("发现 %d 个国家，共 %d 个服务器", len(servers), totalServers)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sessionManager := session.NewManager(cfg.DefaultSessionTTL)
	workerManager := workermgr.New(servers, cfg.AuthFile, sessionManager, cfg.WorkerIdleTimeout, cfg.WorkerMaxLifetime, cfg.MinPoolSize, cfg.WorkerVerbose)
	routerInstance := router.New(workerManager, sessionManager)

	go cleanupSessions(ctx, sessionManager)
	workerManager.StartHealthCheck(ctx)
	workerManager.StartPoolWarmer(ctx)

	socks5Server := proxy.NewSocks5Server(cfg, routerInstance, workerManager)
	httpServer := proxy.NewHTTPProxyServer(cfg, routerInstance, workerManager)

	go func() {
		if err := socks5Server.ListenAndServe(ctx); err != nil {
			log.Printf("SOCKS5 服务退出: %v", err)
			cancel()
		}
	}()

	go func() {
		if err := httpServer.ListenAndServe(ctx); err != nil {
			log.Printf("HTTP 服务退出: %v", err)
			cancel()
		}
	}()

	log.Printf("网关已启动，SOCKS5=%d HTTP=%d", cfg.Socks5Port, cfg.HTTPPort)

	waitForShutdown(cancel)
	workerManager.Shutdown()
}

func enableIPForwarding() {
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644); err != nil {
		log.Printf("警告: 无法启用 IPv4 转发: %v", err)
	}
}

func cleanupSessions(ctx context.Context, manager *session.Manager) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if removed := manager.Cleanup(); removed > 0 {
				log.Printf("已清理 %d 个过期 session", removed)
			}
		}
	}
}

func waitForShutdown(cancel context.CancelFunc) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	<-signals
	cancel()
}
