package config

import (
	"os"
	"strconv"
	"time"
)

// Config 存储网关运行所需的配置。
type Config struct {
	Socks5Port        int
	HTTPPort          int
	ProxyUser         string
	ProxyPass         string
	OvpnDir           string
	AuthFile          string
	DefaultSessionTTL time.Duration
	WorkerIdleTimeout time.Duration
}

// Load 从环境变量读取配置，未设置时回落到默认值。
func Load() Config {
	return Config{
		Socks5Port:        getEnvInt("SOCKS5_PORT", 1080),
		HTTPPort:          getEnvInt("HTTP_PORT", 8888),
		ProxyUser:         getEnvStr("PROXY_USER", "user"),
		ProxyPass:         getEnvStr("PROXY_PASS", "pass"),
		OvpnDir:           getEnvStr("OVPN_DIR", "/etc/openvpn/ovpn"),
		AuthFile:          getEnvStr("AUTH_FILE", "/etc/openvpn/auth.txt"),
		DefaultSessionTTL: time.Duration(getEnvInt("DEFAULT_SESSION_TTL", 30)) * time.Minute,
		WorkerIdleTimeout: time.Duration(getEnvInt("WORKER_IDLE_TIMEOUT", 10)) * time.Minute,
	}
}

func getEnvStr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}

	return fallback
}

func getEnvInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}

	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}

	return parsed
}
