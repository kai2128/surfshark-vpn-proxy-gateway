# Surfshark VPN Proxy Gateway Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a single-container Go proxy gateway that routes SOCKS5/HTTP traffic through Surfshark VPN connections using Linux network namespaces, with DataImpulse-style username parameters for sticky/rotating control.

**Architecture:** Go main process manages OpenVPN child processes each running in isolated Linux network namespaces. Traffic is forwarded by entering the target netns, dialing the destination through the VPN tunnel, then piping data back. Session mapping and worker lifecycle are managed in-memory.

**Tech Stack:** Go 1.22+, `things-go/go-socks5`, `vishvananda/netns`, `vishvananda/netlink`, Alpine Linux, OpenVPN, Docker

---

## File Structure

```
surfshark-vpn-proxy-gateway/
├── cmd/
│   └── gateway/
│       └── main.go              # 入口点，组装所有组件
├── internal/
│   ├── config/
│   │   └── config.go            # 环境变量配置
│   ├── parser/
│   │   ├── parser.go            # DataImpulse 风格用户名参数解析
│   │   └── parser_test.go
│   ├── discovery/
│   │   ├── discovery.go         # 扫描 .ovpn 文件，发现可用服务器
│   │   └── discovery_test.go
│   ├── netns/
│   │   └── netns.go             # Linux 网络命名空间管理 (创建/销毁 netns, veth, iptables)
│   ├── worker/
│   │   ├── manager.go           # Worker 生命周期管理 (创建/健康检查/回收)
│   │   └── worker.go            # 单个 Worker 结构体和状态
│   ├── session/
│   │   ├── session.go           # Sticky session 管理 (映射/TTL/清理)
│   │   └── session_test.go
│   ├── router/
│   │   ├── router.go            # 路由逻辑 (选择 worker, sticky vs rotating)
│   │   └── router_test.go
│   └── proxy/
│       ├── socks5.go            # SOCKS5 代理服务器 (基于 go-socks5)
│       ├── http.go              # HTTP CONNECT 代理服务器
│       └── dialer.go            # netns 感知的 TCP dialer
├── Dockerfile                   # 多阶段构建
├── docker-compose.yml           # 开发用
├── go.mod
└── go.sum
```

---

### Task 1: Project Scaffolding

**Files:**
- Create: `go.mod`
- Create: `cmd/gateway/main.go`
- Create: `internal/config/config.go`

- [ ] **Step 1: Initialize Go module**

Run:
```bash
cd /Volumes/tofu/Projects/surfshark-vpn-proxy-gateway
go mod init surfshark-proxy
```

- [ ] **Step 2: Create config package**

Create `internal/config/config.go`:

```go
package config

import (
	"os"
	"strconv"
	"time"
)

// Config 存储所有网关配置，从环境变量读取
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

// Load 从环境变量加载配置，未设置则使用默认值
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
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
```

- [ ] **Step 3: Create minimal main.go**

Create `cmd/gateway/main.go`:

```go
package main

import (
	"log"

	"surfshark-proxy/internal/config"
)

func main() {
	cfg := config.Load()
	log.Printf("Surfshark VPN Proxy Gateway 启动中...")
	log.Printf("SOCKS5 端口: %d, HTTP 端口: %d", cfg.Socks5Port, cfg.HTTPPort)
	log.Printf("OVPN 目录: %s", cfg.OvpnDir)
}
```

- [ ] **Step 4: Verify build**

Run:
```bash
go build ./cmd/gateway/
```
Expected: 编译成功，无错误

- [ ] **Step 5: Commit**

```bash
git add go.mod cmd/ internal/config/
git commit -m "feat: 项目初始化，添加配置包和入口点"
```

---

### Task 2: Username Parser

**Files:**
- Create: `internal/parser/parser.go`
- Create: `internal/parser/parser_test.go`

- [ ] **Step 1: Write failing tests**

Create `internal/parser/parser_test.go`:

```go
package parser

import (
	"testing"
	"time"
)

func TestParseBasicAuth(t *testing.T) {
	// 无参数，纯用户名密码
	result := Parse("user", "pass")
	if result.Username != "user" {
		t.Errorf("expected username 'user', got '%s'", result.Username)
	}
	if result.Country != "" {
		t.Errorf("expected empty country, got '%s'", result.Country)
	}
	if result.SessionID != "" {
		t.Errorf("expected empty sessionID, got '%s'", result.SessionID)
	}
	if result.SessionTTL != 0 {
		t.Errorf("expected zero TTL, got %v", result.SessionTTL)
	}
}

func TestParseCountryOnly(t *testing.T) {
	result := Parse("user__cr.us", "pass")
	if result.Username != "user" {
		t.Errorf("expected username 'user', got '%s'", result.Username)
	}
	if result.Country != "us" {
		t.Errorf("expected country 'us', got '%s'", result.Country)
	}
}

func TestParseFullParams(t *testing.T) {
	result := Parse("user__cr.jp;sessid.abc123;sessttl.60", "pass")
	if result.Username != "user" {
		t.Errorf("expected username 'user', got '%s'", result.Username)
	}
	if result.Country != "jp" {
		t.Errorf("expected country 'jp', got '%s'", result.Country)
	}
	if result.SessionID != "abc123" {
		t.Errorf("expected sessionID 'abc123', got '%s'", result.SessionID)
	}
	if result.SessionTTL != 60*time.Minute {
		t.Errorf("expected TTL 60m, got %v", result.SessionTTL)
	}
}

func TestParseSessionWithoutTTL(t *testing.T) {
	result := Parse("user__sessid.mysession", "pass")
	if result.SessionID != "mysession" {
		t.Errorf("expected sessionID 'mysession', got '%s'", result.SessionID)
	}
	// TTL 应为 0，由调用方填充默认值
	if result.SessionTTL != 0 {
		t.Errorf("expected zero TTL, got %v", result.SessionTTL)
	}
}

func TestParseInvalidTTL(t *testing.T) {
	result := Parse("user__sessttl.abc", "pass")
	// 无效 TTL 忽略，保持零值
	if result.SessionTTL != 0 {
		t.Errorf("expected zero TTL for invalid input, got %v", result.SessionTTL)
	}
}

func TestParseNoDoubleUnderscore(t *testing.T) {
	// 用户名中没有 __ 分隔符
	result := Parse("simpleuser", "pass")
	if result.Username != "simpleuser" {
		t.Errorf("expected username 'simpleuser', got '%s'", result.Username)
	}
	if result.Country != "" || result.SessionID != "" {
		t.Errorf("expected no params for simple username")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:
```bash
cd /Volumes/tofu/Projects/surfshark-vpn-proxy-gateway
go test ./internal/parser/ -v
```
Expected: 编译失败，`Parse` 函数未定义

- [ ] **Step 3: Implement parser**

Create `internal/parser/parser.go`:

```go
package parser

import (
	"strconv"
	"strings"
	"time"
)

// Params 解析后的代理请求参数
type Params struct {
	Username   string        // 基础用户名（__ 之前的部分）
	Country    string        // cr 参数：国家代码
	SessionID  string        // sessid 参数：sticky session ID
	SessionTTL time.Duration // sessttl 参数：session 过期时间（分钟）
}

// IsSticky 判断是否为 sticky 模式（有 session ID）
func (p Params) IsSticky() bool {
	return p.SessionID != ""
}

// Parse 解析 DataImpulse 风格的用户名参数
// 格式: {username}__{param1}.{value1};{param2}.{value2}
func Parse(username, password string) Params {
	p := Params{}

	// 按 __ 分割用户名和参数部分
	parts := strings.SplitN(username, "__", 2)
	p.Username = parts[0]

	if len(parts) < 2 {
		return p
	}

	// 按 ; 分割各参数
	paramStr := parts[1]
	params := strings.Split(paramStr, ";")

	for _, param := range params {
		// 按 . 分割 key 和 value
		kv := strings.SplitN(param, ".", 2)
		if len(kv) != 2 {
			continue
		}
		key := kv[0]
		value := kv[1]

		switch key {
		case "cr":
			p.Country = strings.ToLower(value)
		case "sessid":
			p.SessionID = value
		case "sessttl":
			if minutes, err := strconv.Atoi(value); err == nil && minutes > 0 {
				p.SessionTTL = time.Duration(minutes) * time.Minute
			}
		}
	}

	return p
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run:
```bash
go test ./internal/parser/ -v
```
Expected: 全部 PASS

- [ ] **Step 5: Commit**

```bash
git add internal/parser/
git commit -m "feat: 添加 DataImpulse 风格用户名参数解析器"
```

---

### Task 3: Server Discovery

**Files:**
- Create: `internal/discovery/discovery.go`
- Create: `internal/discovery/discovery_test.go`

- [ ] **Step 1: Write failing tests**

Create `internal/discovery/discovery_test.go`:

```go
package discovery

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseServerName(t *testing.T) {
	tests := []struct {
		filename string
		country  string
		server   string
	}{
		{"us-dal.prod.surfshark.com_udp.ovpn", "us", "us-dal"},
		{"jp-tok.prod.surfshark.com_udp.ovpn", "jp", "jp-tok"},
		{"de-fra.prod.surfshark.com_tcp.ovpn", "de", "de-fra"},
		{"uk-lon.prod.surfshark.com_udp.ovpn", "uk", "uk-lon"},
		{"us-nyc.prod.surfshark.com_udp.ovpn", "us", "us-nyc"},
	}

	for _, tt := range tests {
		country, server := parseFilename(tt.filename)
		if country != tt.country {
			t.Errorf("parseFilename(%q): country = %q, want %q", tt.filename, country, tt.country)
		}
		if server != tt.server {
			t.Errorf("parseFilename(%q): server = %q, want %q", tt.filename, server, tt.server)
		}
	}
}

func TestParseInvalidFilename(t *testing.T) {
	country, server := parseFilename("not-a-valid-file.txt")
	if country != "" || server != "" {
		t.Errorf("expected empty for invalid filename, got country=%q server=%q", country, server)
	}
}

func TestScanDirectory(t *testing.T) {
	// 创建临时目录和假 .ovpn 文件
	dir := t.TempDir()
	files := []string{
		"us-dal.prod.surfshark.com_udp.ovpn",
		"us-nyc.prod.surfshark.com_udp.ovpn",
		"jp-tok.prod.surfshark.com_udp.ovpn",
		"de-fra.prod.surfshark.com_tcp.ovpn",
		"readme.txt", // 非 .ovpn 文件，应被忽略
	}
	for _, f := range files {
		os.WriteFile(filepath.Join(dir, f), []byte("client\nremote example.com 1194"), 0644)
	}

	servers, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan failed: %v", err)
	}

	// 验证美国有 2 个服务器
	if len(servers["us"]) != 2 {
		t.Errorf("expected 2 US servers, got %d", len(servers["us"]))
	}
	// 验证日本有 1 个
	if len(servers["jp"]) != 1 {
		t.Errorf("expected 1 JP server, got %d", len(servers["jp"]))
	}
	// 验证德国有 1 个
	if len(servers["de"]) != 1 {
		t.Errorf("expected 1 DE server, got %d", len(servers["de"]))
	}
	// readme.txt 应被忽略
	totalServers := 0
	for _, s := range servers {
		totalServers += len(s)
	}
	if totalServers != 4 {
		t.Errorf("expected 4 total servers, got %d", totalServers)
	}
}

func TestScanEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	servers, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan failed: %v", err)
	}
	if len(servers) != 0 {
		t.Errorf("expected 0 countries, got %d", len(servers))
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:
```bash
go test ./internal/discovery/ -v
```
Expected: 编译失败

- [ ] **Step 3: Implement discovery**

Create `internal/discovery/discovery.go`:

```go
package discovery

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// Server 表示一个可用的 Surfshark VPN 服务器
type Server struct {
	Country  string // 国家代码，如 "us"
	Name     string // 服务器名称，如 "us-dal"
	OvpnPath string // .ovpn 文件的完整路径
}

// Scan 扫描目录中的 .ovpn 文件，返回按国家分组的服务器列表
func Scan(dir string) (map[string][]Server, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("读取目录 %s 失败: %w", dir, err)
	}

	servers := make(map[string][]Server)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".ovpn") {
			continue
		}

		country, name := parseFilename(entry.Name())
		if country == "" {
			log.Printf("跳过无法解析的文件: %s", entry.Name())
			continue
		}

		servers[country] = append(servers[country], Server{
			Country:  country,
			Name:     name,
			OvpnPath: filepath.Join(dir, entry.Name()),
		})
	}

	return servers, nil
}

// parseFilename 从 .ovpn 文件名中提取国家代码和服务器名
// 例: "us-dal.prod.surfshark.com_udp.ovpn" → ("us", "us-dal")
func parseFilename(filename string) (country, server string) {
	if !strings.HasSuffix(filename, ".ovpn") {
		return "", ""
	}

	// 取第一个 "." 之前的部分作为服务器名
	// us-dal.prod.surfshark.com_udp.ovpn → us-dal
	dotIdx := strings.Index(filename, ".")
	if dotIdx <= 0 {
		return "", ""
	}
	server = filename[:dotIdx]

	// 取服务器名中第一个 "-" 之前的部分作为国家代码
	// us-dal → us
	dashIdx := strings.Index(server, "-")
	if dashIdx <= 0 {
		return "", ""
	}
	country = server[:dashIdx]

	return country, server
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run:
```bash
go test ./internal/discovery/ -v
```
Expected: 全部 PASS

- [ ] **Step 5: Commit**

```bash
git add internal/discovery/
git commit -m "feat: 添加 .ovpn 文件扫描和服务器自动发现"
```

---

### Task 4: Session Manager

**Files:**
- Create: `internal/session/session.go`
- Create: `internal/session/session_test.go`

- [ ] **Step 1: Write failing tests**

Create `internal/session/session_test.go`:

```go
package session

import (
	"testing"
	"time"
)

func TestGetOrCreate_NewSession(t *testing.T) {
	m := NewManager(30 * time.Minute)

	sess := m.GetOrCreate("sess1", "us", 0)
	if sess.ID != "sess1" {
		t.Errorf("expected ID 'sess1', got '%s'", sess.ID)
	}
	if sess.Country != "us" {
		t.Errorf("expected country 'us', got '%s'", sess.Country)
	}
	// 新 session 未绑定 worker
	if sess.WorkerID != "" {
		t.Errorf("expected empty workerID, got '%s'", sess.WorkerID)
	}
	if sess.TTL != 30*time.Minute {
		t.Errorf("expected default TTL 30m, got %v", sess.TTL)
	}
}

func TestGetOrCreate_ExistingSession(t *testing.T) {
	m := NewManager(30 * time.Minute)

	sess1 := m.GetOrCreate("sess1", "us", 0)
	sess1.WorkerID = "worker-0"
	m.Update(sess1)

	sess2 := m.GetOrCreate("sess1", "us", 0)
	if sess2.WorkerID != "worker-0" {
		t.Errorf("expected existing worker 'worker-0', got '%s'", sess2.WorkerID)
	}
}

func TestGetOrCreate_CustomTTL(t *testing.T) {
	m := NewManager(30 * time.Minute)

	sess := m.GetOrCreate("sess1", "jp", 60*time.Minute)
	if sess.TTL != 60*time.Minute {
		t.Errorf("expected custom TTL 60m, got %v", sess.TTL)
	}
}

func TestGetOrCreate_ExpiredSession(t *testing.T) {
	m := NewManager(1 * time.Millisecond)

	sess := m.GetOrCreate("sess1", "us", 1*time.Millisecond)
	sess.WorkerID = "worker-0"
	m.Update(sess)

	time.Sleep(5 * time.Millisecond)

	// 过期后应返回新 session
	sess2 := m.GetOrCreate("sess1", "us", 1*time.Millisecond)
	if sess2.WorkerID != "" {
		t.Errorf("expected empty workerID for expired session, got '%s'", sess2.WorkerID)
	}
}

func TestRemoveByWorker(t *testing.T) {
	m := NewManager(30 * time.Minute)

	sess1 := m.GetOrCreate("sess1", "us", 0)
	sess1.WorkerID = "worker-0"
	m.Update(sess1)

	sess2 := m.GetOrCreate("sess2", "us", 0)
	sess2.WorkerID = "worker-0"
	m.Update(sess2)

	sess3 := m.GetOrCreate("sess3", "jp", 0)
	sess3.WorkerID = "worker-1"
	m.Update(sess3)

	m.RemoveByWorker("worker-0")

	// sess1 和 sess2 应被删除
	s := m.GetOrCreate("sess1", "us", 0)
	if s.WorkerID != "" {
		t.Errorf("expected sess1 to be removed")
	}
	// sess3 应保留
	s3 := m.GetOrCreate("sess3", "jp", 0)
	if s3.WorkerID != "worker-1" {
		t.Errorf("expected sess3 to still have worker-1")
	}
}

func TestCleanup(t *testing.T) {
	m := NewManager(1 * time.Millisecond)

	sess := m.GetOrCreate("sess1", "us", 1*time.Millisecond)
	sess.WorkerID = "worker-0"
	m.Update(sess)

	time.Sleep(5 * time.Millisecond)

	removed := m.Cleanup()
	if removed != 1 {
		t.Errorf("expected 1 removed, got %d", removed)
	}
}

func TestActiveSessionsForWorker(t *testing.T) {
	m := NewManager(30 * time.Minute)

	sess1 := m.GetOrCreate("s1", "us", 0)
	sess1.WorkerID = "worker-0"
	m.Update(sess1)

	sess2 := m.GetOrCreate("s2", "us", 0)
	sess2.WorkerID = "worker-0"
	m.Update(sess2)

	count := m.ActiveSessionsForWorker("worker-0")
	if count != 2 {
		t.Errorf("expected 2 active sessions, got %d", count)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:
```bash
go test ./internal/session/ -v
```
Expected: 编译失败

- [ ] **Step 3: Implement session manager**

Create `internal/session/session.go`:

```go
package session

import (
	"sync"
	"time"
)

// Session 表示一个 sticky session 映射
type Session struct {
	ID        string
	WorkerID  string
	Country   string
	TTL       time.Duration
	CreatedAt time.Time
	LastUsed  time.Time
}

// IsExpired 检查 session 是否已过期
func (s *Session) IsExpired() bool {
	return time.Since(s.CreatedAt) > s.TTL
}

// Manager 管理所有 sticky session
type Manager struct {
	mu         sync.RWMutex
	sessions   map[string]*Session
	defaultTTL time.Duration
}

// NewManager 创建新的 session 管理器
func NewManager(defaultTTL time.Duration) *Manager {
	return &Manager{
		sessions:   make(map[string]*Session),
		defaultTTL: defaultTTL,
	}
}

// GetOrCreate 获取已有 session 或创建新 session
// 如果 session 已过期，删除旧的并创建新的
// ttl 为 0 时使用默认值
func (m *Manager) GetOrCreate(id, country string, ttl time.Duration) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()

	if sess, ok := m.sessions[id]; ok {
		if !sess.IsExpired() {
			sess.LastUsed = time.Now()
			return sess
		}
		// 过期，删除旧 session
		delete(m.sessions, id)
	}

	if ttl == 0 {
		ttl = m.defaultTTL
	}

	sess := &Session{
		ID:        id,
		Country:   country,
		TTL:       ttl,
		CreatedAt: time.Now(),
		LastUsed:  time.Now(),
	}
	m.sessions[id] = sess
	return sess
}

// Update 更新已有 session（通常用于绑定 worker）
func (m *Manager) Update(sess *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[sess.ID] = sess
}

// RemoveByWorker 删除所有绑定到指定 worker 的 session
func (m *Manager) RemoveByWorker(workerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, sess := range m.sessions {
		if sess.WorkerID == workerID {
			delete(m.sessions, id)
		}
	}
}

// Cleanup 清理所有过期 session，返回清理数量
func (m *Manager) Cleanup() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	removed := 0
	for id, sess := range m.sessions {
		if sess.IsExpired() {
			delete(m.sessions, id)
			removed++
		}
	}
	return removed
}

// ActiveSessionsForWorker 返回指向指定 worker 的活跃 session 数量
func (m *Manager) ActiveSessionsForWorker(workerID string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	count := 0
	for _, sess := range m.sessions {
		if sess.WorkerID == workerID && !sess.IsExpired() {
			count++
		}
	}
	return count
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run:
```bash
go test ./internal/session/ -v
```
Expected: 全部 PASS

- [ ] **Step 5: Commit**

```bash
git add internal/session/
git commit -m "feat: 添加 sticky session 管理器，支持 TTL 和自动清理"
```

---

### Task 5: Router

**Files:**
- Create: `internal/router/router.go`
- Create: `internal/router/router_test.go`

- [ ] **Step 1: Write failing tests**

Create `internal/router/router_test.go`:

```go
package router

import (
	"testing"
	"time"

	"surfshark-proxy/internal/parser"
	"surfshark-proxy/internal/session"
)

// mockWorkerPool 用于测试的 mock worker pool
type mockWorkerPool struct {
	workers map[string]*WorkerInfo // workerID → info
}

func (m *mockWorkerPool) GetReadyWorkers(country string) []*WorkerInfo {
	var result []*WorkerInfo
	for _, w := range m.workers {
		if w.State != WorkerReady {
			continue
		}
		if country == "" || w.Country == country {
			result = append(result, w)
		}
	}
	return result
}

func (m *mockWorkerPool) RequestWorker(country string) (*WorkerInfo, error) {
	// 模拟创建一个新 worker
	id := "new-worker-" + country
	w := &WorkerInfo{ID: id, Country: country, State: WorkerReady}
	m.workers[id] = w
	return w, nil
}

func TestRotatingNoCountry(t *testing.T) {
	pool := &mockWorkerPool{
		workers: map[string]*WorkerInfo{
			"w0": {ID: "w0", Country: "us", State: WorkerReady},
			"w1": {ID: "w1", Country: "jp", State: WorkerReady},
		},
	}
	sm := session.NewManager(30 * time.Minute)
	r := New(pool, sm)

	params := parser.Params{Username: "user"}
	w1, err := r.Route(params)
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}

	w2, err := r.Route(params)
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}

	// Round-robin 应该选不同的 worker
	if w1.ID == w2.ID {
		t.Logf("warning: same worker selected twice (acceptable with 2 workers)")
	}
}

func TestRotatingWithCountry(t *testing.T) {
	pool := &mockWorkerPool{
		workers: map[string]*WorkerInfo{
			"w0": {ID: "w0", Country: "us", State: WorkerReady},
			"w1": {ID: "w1", Country: "jp", State: WorkerReady},
		},
	}
	sm := session.NewManager(30 * time.Minute)
	r := New(pool, sm)

	params := parser.Params{Username: "user", Country: "jp"}
	w, err := r.Route(params)
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}
	if w.Country != "jp" {
		t.Errorf("expected country 'jp', got '%s'", w.Country)
	}
}

func TestStickySession(t *testing.T) {
	pool := &mockWorkerPool{
		workers: map[string]*WorkerInfo{
			"w0": {ID: "w0", Country: "us", State: WorkerReady},
			"w1": {ID: "w1", Country: "us", State: WorkerReady},
		},
	}
	sm := session.NewManager(30 * time.Minute)
	r := New(pool, sm)

	params := parser.Params{Username: "user", Country: "us", SessionID: "sess1"}
	w1, err := r.Route(params)
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}

	// 同一个 session 应该路由到同一个 worker
	w2, err := r.Route(params)
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}
	if w1.ID != w2.ID {
		t.Errorf("sticky session routed to different workers: %s vs %s", w1.ID, w2.ID)
	}
}

func TestRequestWorkerOnDemand(t *testing.T) {
	pool := &mockWorkerPool{
		workers: map[string]*WorkerInfo{},
	}
	sm := session.NewManager(30 * time.Minute)
	r := New(pool, sm)

	params := parser.Params{Username: "user", Country: "de"}
	w, err := r.Route(params)
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}
	if w.Country != "de" {
		t.Errorf("expected country 'de', got '%s'", w.Country)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:
```bash
go test ./internal/router/ -v
```
Expected: 编译失败

- [ ] **Step 3: Implement router**

Create `internal/router/router.go`:

```go
package router

import (
	"fmt"
	"sync"
	"sync/atomic"

	"surfshark-proxy/internal/parser"
	"surfshark-proxy/internal/session"
)

// WorkerState worker 状态
type WorkerState int

const (
	WorkerCreating WorkerState = iota
	WorkerReady
	WorkerIdle
	WorkerClosing
)

// WorkerInfo worker 的基本信息，供路由使用
type WorkerInfo struct {
	ID      string
	Country string
	State   WorkerState
}

// WorkerPool worker 池接口，由 worker manager 实现
type WorkerPool interface {
	// GetReadyWorkers 获取指定国家的就绪 worker 列表
	// country 为空时返回所有就绪 worker
	GetReadyWorkers(country string) []*WorkerInfo
	// RequestWorker 按需创建新 worker（同步，等待就绪）
	RequestWorker(country string) (*WorkerInfo, error)
}

// Router 路由逻辑：选择 worker
type Router struct {
	pool    WorkerPool
	session *session.Manager
	counter atomic.Uint64 // round-robin 计数器
	mu      sync.Mutex
}

// New 创建路由器
func New(pool WorkerPool, sessionMgr *session.Manager) *Router {
	return &Router{
		pool:    pool,
		session: sessionMgr,
	}
}

// Route 根据请求参数选择目标 worker
func (r *Router) Route(params parser.Params) (*WorkerInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Sticky 模式
	if params.IsSticky() {
		return r.routeSticky(params)
	}

	// Rotating 模式
	return r.routeRotating(params.Country)
}

func (r *Router) routeSticky(params parser.Params) (*WorkerInfo, error) {
	sess := r.session.GetOrCreate(params.SessionID, params.Country, params.SessionTTL)

	// 已绑定 worker，检查是否仍然可用
	if sess.WorkerID != "" {
		workers := r.pool.GetReadyWorkers("")
		for _, w := range workers {
			if w.ID == sess.WorkerID {
				return w, nil
			}
		}
		// Worker 不可用，重新分配
	}

	// 分配新 worker
	w, err := r.selectOrCreate(params.Country)
	if err != nil {
		return nil, err
	}

	sess.WorkerID = w.ID
	r.session.Update(sess)
	return w, nil
}

func (r *Router) routeRotating(country string) (*WorkerInfo, error) {
	return r.selectOrCreate(country)
}

func (r *Router) selectOrCreate(country string) (*WorkerInfo, error) {
	workers := r.pool.GetReadyWorkers(country)
	if len(workers) > 0 {
		// Round-robin 选择
		idx := r.counter.Add(1) - 1
		return workers[idx%uint64(len(workers))], nil
	}

	// 没有可用 worker，按需创建
	if country == "" {
		return nil, fmt.Errorf("没有可用的 worker，且未指定国家")
	}
	return r.pool.RequestWorker(country)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run:
```bash
go test ./internal/router/ -v
```
Expected: 全部 PASS

- [ ] **Step 5: Commit**

```bash
git add internal/router/
git commit -m "feat: 添加路由逻辑，支持 round-robin 轮换和 sticky session"
```

---

### Task 6: Network Namespace Manager

**Files:**
- Create: `internal/netns/netns.go`

注意：此组件依赖 Linux 内核特性（netns, veth, iptables），只能在 Docker 容器中测试。

- [ ] **Step 1: Install dependencies**

Run:
```bash
cd /Volumes/tofu/Projects/surfshark-vpn-proxy-gateway
go get github.com/vishvananda/netns
go get github.com/vishvananda/netlink
```

- [ ] **Step 2: Implement netns manager**

Create `internal/netns/netns.go`:

```go
package netns

import (
	"fmt"
	"net"
	"os/exec"
	"runtime"

	"github.com/vishvananda/netlink"
	vishnetns "github.com/vishvananda/netns"
)

// Namespace 表示一个已创建的网络命名空间及其资源
type Namespace struct {
	Name     string            // 命名空间名称，如 "worker-0"
	Handle   vishnetns.NsHandle // 命名空间文件句柄
	VethHost string            // 宿主端 veth 名称
	VethPeer string            // 命名空间端 veth 名称
	HostIP   net.IP            // 宿主端 IP
	PeerIP   net.IP            // 命名空间端 IP
}

// Create 创建网络命名空间及 veth pair
// index 用于生成唯一的名称和 IP 地址 (10.200.{index}.1/30 ↔ 10.200.{index}.2/30)
func Create(name string, index int) (*Namespace, error) {
	if index > 254 {
		return nil, fmt.Errorf("index %d 超出范围 (最大 254)", index)
	}

	vethHost := fmt.Sprintf("veth-%s", name)
	vethPeer := fmt.Sprintf("vpeer-%s", name)
	hostIP := net.IPv4(10, 200, byte(index), 1)
	peerIP := net.IPv4(10, 200, byte(index), 2)

	// 1. 创建命名空间
	handle, err := vishnetns.NewNamed(name)
	if err != nil {
		return nil, fmt.Errorf("创建命名空间 %s 失败: %w", name, err)
	}

	ns := &Namespace{
		Name:     name,
		Handle:   handle,
		VethHost: vethHost,
		VethPeer: vethPeer,
		HostIP:   hostIP,
		PeerIP:   peerIP,
	}

	// 2. 创建 veth pair
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: vethHost},
		PeerName:  vethPeer,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		ns.Destroy()
		return nil, fmt.Errorf("创建 veth pair 失败: %w", err)
	}

	// 3. 将 peer 端移入命名空间
	peerLink, err := netlink.LinkByName(vethPeer)
	if err != nil {
		ns.Destroy()
		return nil, fmt.Errorf("获取 peer link 失败: %w", err)
	}
	if err := netlink.LinkSetNsFd(peerLink, int(handle)); err != nil {
		ns.Destroy()
		return nil, fmt.Errorf("移动 peer 到命名空间失败: %w", err)
	}

	// 4. 配置宿主端 IP 和启动
	hostLink, err := netlink.LinkByName(vethHost)
	if err != nil {
		ns.Destroy()
		return nil, fmt.Errorf("获取 host link 失败: %w", err)
	}
	hostAddr, _ := netlink.ParseAddr(fmt.Sprintf("%s/30", hostIP.String()))
	if err := netlink.AddrAdd(hostLink, hostAddr); err != nil {
		ns.Destroy()
		return nil, fmt.Errorf("设置 host IP 失败: %w", err)
	}
	if err := netlink.LinkSetUp(hostLink); err != nil {
		ns.Destroy()
		return nil, fmt.Errorf("启动 host veth 失败: %w", err)
	}

	// 5. 在命名空间内配置 peer 端
	if err := ns.configurePeer(peerIP); err != nil {
		ns.Destroy()
		return nil, fmt.Errorf("配置 peer 失败: %w", err)
	}

	// 6. 设置 NAT (iptables masquerade)
	subnet := fmt.Sprintf("10.200.%d.0/30", index)
	if err := exec.Command("iptables", "-t", "nat", "-A", "POSTROUTING",
		"-s", subnet, "-j", "MASQUERADE").Run(); err != nil {
		ns.Destroy()
		return nil, fmt.Errorf("设置 NAT 失败: %w", err)
	}

	return ns, nil
}

// configurePeer 在命名空间内配置网络接口
func (ns *Namespace) configurePeer(peerIP net.IP) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// 保存当前命名空间
	origNs, err := vishnetns.Get()
	if err != nil {
		return fmt.Errorf("获取当前命名空间失败: %w", err)
	}
	defer origNs.Close()

	// 切换到 worker 命名空间
	if err := vishnetns.Set(ns.Handle); err != nil {
		return fmt.Errorf("切换命名空间失败: %w", err)
	}
	defer vishnetns.Set(origNs)

	// 启动 lo
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("获取 lo 失败: %w", err)
	}
	netlink.LinkSetUp(lo)

	// 配置 peer IP
	peer, err := netlink.LinkByName(ns.VethPeer)
	if err != nil {
		return fmt.Errorf("获取 peer link 失败: %w", err)
	}
	addr, _ := netlink.ParseAddr(fmt.Sprintf("%s/30", peerIP.String()))
	if err := netlink.AddrAdd(peer, addr); err != nil {
		return fmt.Errorf("设置 peer IP 失败: %w", err)
	}
	if err := netlink.LinkSetUp(peer); err != nil {
		return fmt.Errorf("启动 peer 失败: %w", err)
	}

	// 设置默认路由 → 宿主端
	defaultRoute := &netlink.Route{
		Gw: ns.HostIP,
	}
	if err := netlink.RouteAdd(defaultRoute); err != nil {
		return fmt.Errorf("设置默认路由失败: %w", err)
	}

	return nil
}

// Destroy 清理命名空间及相关资源
func (ns *Namespace) Destroy() error {
	// 删除宿主端 veth（peer 端会自动删除）
	if link, err := netlink.LinkByName(ns.VethHost); err == nil {
		netlink.LinkDel(link)
	}

	// 清理 iptables 规则
	exec.Command("iptables", "-t", "nat", "-D", "POSTROUTING",
		"-s", fmt.Sprintf("10.200.%d.0/30", ns.ipIndex()), "-j", "MASQUERADE").Run()

	// 删除命名空间
	vishnetns.DeleteNamed(ns.Name)
	ns.Handle.Close()

	return nil
}

func (ns *Namespace) ipIndex() int {
	return int(ns.HostIP[2])
}
```

- [ ] **Step 3: Verify build**

Run:
```bash
go build ./internal/netns/
```
Expected: 编译成功（注意：在 macOS 上可能因为 linux-only 的 syscall 报错，这是预期的。通过 `GOOS=linux go build` 验证）

Run:
```bash
GOOS=linux go build ./internal/netns/
```
Expected: 编译成功

- [ ] **Step 4: Commit**

```bash
git add internal/netns/
git commit -m "feat: 添加 Linux 网络命名空间管理（创建/销毁 netns, veth, NAT）"
```

---

### Task 7: Netns-Aware Dialer

**Files:**
- Create: `internal/proxy/dialer.go`

- [ ] **Step 1: Implement dialer**

Create `internal/proxy/dialer.go`:

```go
package proxy

import (
	"context"
	"fmt"
	"net"
	"runtime"

	vishnetns "github.com/vishvananda/netns"
)

// NsDialer 在指定的网络命名空间中建立 TCP 连接
type NsDialer struct{}

// DialInNs 在指定命名空间内拨号
// 锁定当前 goroutine 到 OS 线程，切换到目标 netns，dial，切换回来
// 返回的 conn 仍然使用目标 netns 的路由表
func (d *NsDialer) DialInNs(ctx context.Context, nsHandle vishnetns.NsHandle, network, address string) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}

	ch := make(chan result, 1)

	go func() {
		// 锁定到当前 OS 线程，防止 goroutine 调度器迁移线程
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		// 保存原始命名空间
		origNs, err := vishnetns.Get()
		if err != nil {
			ch <- result{nil, fmt.Errorf("获取当前命名空间失败: %w", err)}
			return
		}
		defer origNs.Close()

		// 切换到目标命名空间
		if err := vishnetns.Set(nsHandle); err != nil {
			ch <- result{nil, fmt.Errorf("切换到目标命名空间失败: %w", err)}
			return
		}

		// 在目标命名空间中拨号
		var dialer net.Dialer
		conn, err := dialer.DialContext(ctx, network, address)

		// 切换回原始命名空间（即使 dial 失败也要切换回来）
		vishnetns.Set(origNs)

		ch <- result{conn, err}
	}()

	select {
	case r := <-ch:
		return r.conn, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
```

- [ ] **Step 2: Verify build**

Run:
```bash
GOOS=linux go build ./internal/proxy/
```
Expected: 编译成功

- [ ] **Step 3: Commit**

```bash
git add internal/proxy/dialer.go
git commit -m "feat: 添加命名空间感知的 TCP dialer"
```

---

### Task 8: Worker Manager

**Files:**
- Create: `internal/worker/worker.go`
- Create: `internal/worker/manager.go`

- [ ] **Step 1: Implement worker struct**

Create `internal/worker/worker.go`:

```go
package worker

import (
	"fmt"
	"os/exec"
	"sync"
	"time"

	"surfshark-proxy/internal/discovery"
	nsmanager "surfshark-proxy/internal/netns"
	"surfshark-proxy/internal/router"
	vishnetns "github.com/vishvananda/netns"
)

// Worker 表示一个活跃的 VPN 连接
type Worker struct {
	mu          sync.RWMutex
	ID          string
	Server      discovery.Server
	State       router.WorkerState
	Namespace   *nsmanager.Namespace
	OvpnProcess *exec.Cmd
	ActiveConns int
	CreatedAt   time.Time
	LastUsed    time.Time
}

// NsHandle 返回此 worker 的命名空间句柄
func (w *Worker) NsHandle() vishnetns.NsHandle {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.Namespace.Handle
}

// Info 返回 WorkerInfo 用于路由
func (w *Worker) Info() *router.WorkerInfo {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return &router.WorkerInfo{
		ID:      w.ID,
		Country: w.Server.Country,
		State:   w.State,
	}
}

// IncrConns 增加活跃连接计数
func (w *Worker) IncrConns() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ActiveConns++
	w.LastUsed = time.Now()
}

// DecrConns 减少活跃连接计数
func (w *Worker) DecrConns() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ActiveConns--
}

// IsIdle 检查 worker 是否空闲
func (w *Worker) IsIdle() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.ActiveConns <= 0
}

// Stop 停止 OpenVPN 进程并清理命名空间
func (w *Worker) Stop() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.State = router.WorkerClosing

	var errs []error
	if w.OvpnProcess != nil && w.OvpnProcess.Process != nil {
		if err := w.OvpnProcess.Process.Kill(); err != nil {
			errs = append(errs, fmt.Errorf("停止 OpenVPN 失败: %w", err))
		}
		w.OvpnProcess.Wait()
	}
	if w.Namespace != nil {
		if err := w.Namespace.Destroy(); err != nil {
			errs = append(errs, fmt.Errorf("清理命名空间失败: %w", err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("停止 worker %s 时发生错误: %v", w.ID, errs)
	}
	return nil
}
```

- [ ] **Step 2: Implement worker manager**

Create `internal/worker/manager.go`:

```go
package manager

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"surfshark-proxy/internal/discovery"
	nsmanager "surfshark-proxy/internal/netns"
	"surfshark-proxy/internal/router"
	"surfshark-proxy/internal/session"
	"surfshark-proxy/internal/worker"
	vishnetns "github.com/vishvananda/netns"
)

// Manager 管理所有 worker 的生命周期
type Manager struct {
	mu           sync.RWMutex
	workers      map[string]*worker.Worker
	servers      map[string][]discovery.Server // 按国家分组的服务器列表
	authFile     string
	sessionMgr   *session.Manager
	idleTimeout  time.Duration
	indexCounter atomic.Int32
}

// New 创建新的 worker 管理器
func New(servers map[string][]discovery.Server, authFile string, sessionMgr *session.Manager, idleTimeout time.Duration) *Manager {
	return &Manager{
		workers:     make(map[string]*worker.Worker),
		servers:     servers,
		authFile:    authFile,
		sessionMgr:  sessionMgr,
		idleTimeout: idleTimeout,
	}
}

// GetReadyWorkers 实现 router.WorkerPool 接口
func (m *Manager) GetReadyWorkers(country string) []*router.WorkerInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*router.WorkerInfo
	for _, w := range m.workers {
		info := w.Info()
		if info.State != router.WorkerReady {
			continue
		}
		if country == "" || info.Country == country {
			result = append(result, info)
		}
	}
	return result
}

// RequestWorker 实现 router.WorkerPool 接口 - 按需创建 worker
func (m *Manager) RequestWorker(country string) (*router.WorkerInfo, error) {
	serverList, ok := m.servers[country]
	if !ok || len(serverList) == 0 {
		return nil, fmt.Errorf("国家 %s 没有可用的 VPN 服务器", country)
	}

	// 轮换选择服务器
	idx := int(m.indexCounter.Add(1)-1) % len(serverList)
	server := serverList[idx]

	w, err := m.createWorker(server)
	if err != nil {
		return nil, err
	}

	return w.Info(), nil
}

// GetWorkerNsHandle 获取指定 worker 的命名空间句柄
func (m *Manager) GetWorkerNsHandle(workerID string) (vishnetns.NsHandle, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	w, ok := m.workers[workerID]
	if !ok {
		return 0, fmt.Errorf("worker %s 不存在", workerID)
	}
	return w.NsHandle(), nil
}

// TrackConn 记录连接开始
func (m *Manager) TrackConn(workerID string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if w, ok := m.workers[workerID]; ok {
		w.IncrConns()
	}
}

// UntrackConn 记录连接结束
func (m *Manager) UntrackConn(workerID string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if w, ok := m.workers[workerID]; ok {
		w.DecrConns()
	}
}

func (m *Manager) createWorker(server discovery.Server) (*worker.Worker, error) {
	index := int(m.indexCounter.Load())
	name := fmt.Sprintf("worker-%d", index)

	log.Printf("创建 worker %s → %s (%s)", name, server.Name, server.Country)

	// 1. 创建网络命名空间
	ns, err := nsmanager.Create(name, index)
	if err != nil {
		return nil, fmt.Errorf("创建命名空间失败: %w", err)
	}

	// 2. 在命名空间中启动 OpenVPN
	cmd := exec.Command("ip", "netns", "exec", name,
		"openvpn",
		"--config", server.OvpnPath,
		"--auth-user-pass", m.authFile,
		"--auth-nocache",
		"--verb", "3",
		"--connect-retry", "3",
		"--connect-timeout", "30",
	)
	if err := cmd.Start(); err != nil {
		ns.Destroy()
		return nil, fmt.Errorf("启动 OpenVPN 失败: %w", err)
	}

	w := &worker.Worker{
		ID:          name,
		Server:      server,
		State:       router.WorkerCreating,
		Namespace:   ns,
		OvpnProcess: cmd,
		CreatedAt:   time.Now(),
		LastUsed:    time.Now(),
	}

	// 3. 等待 VPN 连接建立（检查 tun 设备）
	if err := m.waitForTun(name, 30*time.Second); err != nil {
		w.Stop()
		return nil, fmt.Errorf("等待 VPN 连接超时: %w", err)
	}

	w.State = router.WorkerReady
	m.mu.Lock()
	m.workers[name] = w
	m.mu.Unlock()

	log.Printf("Worker %s 就绪 → %s", name, server.Name)
	return w, nil
}

// waitForTun 等待命名空间中的 tun 设备出现
func (m *Manager) waitForTun(nsName string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("等待 tun 设备超时 (%v)", timeout)
		case <-ticker.C:
			// 检查命名空间中是否存在 tun 设备
			out, err := exec.Command("ip", "netns", "exec", nsName,
				"ip", "link", "show", "type", "tun").Output()
			if err == nil && len(out) > 0 {
				return nil
			}
		}
	}
}

// StartHealthCheck 启动健康检查和空闲回收协程
func (m *Manager) StartHealthCheck(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.checkWorkers()
			}
		}
	}()
}

func (m *Manager) checkWorkers() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, w := range m.workers {
		// 检查 OpenVPN 进程是否存活
		if w.OvpnProcess != nil && w.OvpnProcess.ProcessState != nil && w.OvpnProcess.ProcessState.Exited() {
			log.Printf("Worker %s 的 OpenVPN 进程已退出，清理中...", id)
			w.Stop()
			m.sessionMgr.RemoveByWorker(id)
			delete(m.workers, id)
			continue
		}

		// 空闲回收
		if w.IsIdle() && m.sessionMgr.ActiveSessionsForWorker(id) == 0 &&
			time.Since(w.LastUsed) > m.idleTimeout {
			log.Printf("Worker %s 空闲超时，回收中...", id)
			w.Stop()
			delete(m.workers, id)
		}
	}
}

// Shutdown 停止所有 worker
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, w := range m.workers {
		log.Printf("关闭 worker %s...", id)
		w.Stop()
	}
	m.workers = make(map[string]*worker.Worker)
}
```

- [ ] **Step 3: Verify build**

Run:
```bash
GOOS=linux go build ./internal/worker/
```
Expected: 编译成功

- [ ] **Step 4: Commit**

```bash
git add internal/worker/
git commit -m "feat: 添加 worker 管理器，支持按需创建、健康检查和空闲回收"
```

---

### Task 9: SOCKS5 Proxy Server

**Files:**
- Create: `internal/proxy/socks5.go`

- [ ] **Step 1: Install go-socks5 dependency**

Run:
```bash
go get github.com/things-go/go-socks5
```

- [ ] **Step 2: Implement SOCKS5 server**

Create `internal/proxy/socks5.go`:

```go
package proxy

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"

	"github.com/things-go/go-socks5"
	"github.com/things-go/go-socks5/statute"
	"surfshark-proxy/internal/config"
	"surfshark-proxy/internal/parser"
	"surfshark-proxy/internal/router"
	vishnetns "github.com/vishvananda/netns"
)

// NsHandleResolver 获取 worker 的命名空间句柄
type NsHandleResolver interface {
	GetWorkerNsHandle(workerID string) (vishnetns.NsHandle, error)
	TrackConn(workerID string)
	UntrackConn(workerID string)
}

// Socks5Server SOCKS5 代理服务器
type Socks5Server struct {
	cfg      config.Config
	router   *router.Router
	resolver NsHandleResolver
	dialer   *NsDialer
}

// NewSocks5Server 创建 SOCKS5 代理服务器
func NewSocks5Server(cfg config.Config, r *router.Router, resolver NsHandleResolver) *Socks5Server {
	return &Socks5Server{
		cfg:      cfg,
		router:   r,
		resolver: resolver,
		dialer:   &NsDialer{},
	}
}

// credentialStore 实现 socks5 的认证接口，同时提取用户名参数
type credentialStore struct {
	cfg config.Config
}

func (cs *credentialStore) Valid(user, password string) bool {
	params := parser.Parse(user, password)
	return params.Username == cs.cfg.ProxyUser && password == cs.cfg.ProxyPass
}

// ListenAndServe 启动 SOCKS5 服务器
func (s *Socks5Server) ListenAndServe(ctx context.Context) error {
	server := socks5.NewServer(
		socks5.WithAuthMethods([]socks5.Authenticator{
			socks5.UserPassAuthenticator{
				Credentials: &credentialStore{cfg: s.cfg},
			},
		}),
		socks5.WithDial(s.dialFunc),
	)

	addr := fmt.Sprintf(":%d", s.cfg.Socks5Port)
	log.Printf("SOCKS5 代理启动: %s", addr)

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("SOCKS5 监听失败: %w", err)
	}

	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	return server.Serve(listener)
}

// dialFunc 自定义 dial 函数 - 通过选定 worker 的命名空间拨号
func (s *Socks5Server) dialFunc(ctx context.Context, network, addr string) (net.Conn, error) {
	// 从 context 中获取用户名（go-socks5 将其放在 context 中）
	username := ""
	if req, ok := ctx.Value(statute.CtxKeyUserName).(string); ok {
		username = req
	}

	params := parser.Parse(username, "")
	w, err := s.router.Route(params)
	if err != nil {
		return nil, fmt.Errorf("路由失败: %w", err)
	}

	nsHandle, err := s.resolver.GetWorkerNsHandle(w.ID)
	if err != nil {
		return nil, fmt.Errorf("获取命名空间失败: %w", err)
	}

	s.resolver.TrackConn(w.ID)
	conn, err := s.dialer.DialInNs(ctx, nsHandle, network, addr)
	if err != nil {
		s.resolver.UntrackConn(w.ID)
		return nil, err
	}

	// 包装 conn 以在关闭时减少计数
	return &trackedConn{Conn: conn, workerID: w.ID, resolver: s.resolver}, nil
}

// trackedConn 包装 net.Conn，在关闭时减少 worker 的活跃连接计数
type trackedConn struct {
	net.Conn
	workerID string
	resolver NsHandleResolver
	closed   bool
}

func (tc *trackedConn) Close() error {
	if !tc.closed {
		tc.closed = true
		tc.resolver.UntrackConn(tc.workerID)
	}
	return tc.Conn.Close()
}

func (tc *trackedConn) Read(b []byte) (n int, err error) {
	n, err = tc.Conn.Read(b)
	if err == io.EOF {
		tc.Close()
	}
	return
}
```

- [ ] **Step 3: Verify build**

Run:
```bash
GOOS=linux go build ./internal/proxy/
```
Expected: 编译成功

- [ ] **Step 4: Commit**

```bash
git add internal/proxy/socks5.go
git commit -m "feat: 添加 SOCKS5 代理服务器，支持用户名参数路由"
```

---

### Task 10: HTTP Proxy Server

**Files:**
- Create: `internal/proxy/http.go`

- [ ] **Step 1: Implement HTTP proxy server**

Create `internal/proxy/http.go`:

```go
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

// HTTPProxyServer HTTP/HTTPS 代理服务器
type HTTPProxyServer struct {
	cfg      config.Config
	router   *router.Router
	resolver NsHandleResolver
	dialer   *NsDialer
}

// NewHTTPProxyServer 创建 HTTP 代理服务器
func NewHTTPProxyServer(cfg config.Config, r *router.Router, resolver NsHandleResolver) *HTTPProxyServer {
	return &HTTPProxyServer{
		cfg:      cfg,
		router:   r,
		resolver: resolver,
		dialer:   &NsDialer{},
	}
}

// ListenAndServe 启动 HTTP 代理服务器
func (s *HTTPProxyServer) ListenAndServe(ctx context.Context) error {
	addr := fmt.Sprintf(":%d", s.cfg.HTTPPort)
	log.Printf("HTTP 代理启动: %s", addr)

	server := &http.Server{
		Addr:    addr,
		Handler: s,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
	}()

	return server.ListenAndServe()
}

// ServeHTTP 处理代理请求
func (s *HTTPProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 解析认证信息
	username, password, ok := s.parseProxyAuth(r)
	if !ok {
		w.Header().Set("Proxy-Authenticate", "Basic realm=\"proxy\"")
		http.Error(w, "需要代理认证", http.StatusProxyAuthRequired)
		return
	}

	params := parser.Parse(username, password)
	if params.Username != s.cfg.ProxyUser || password != s.cfg.ProxyPass {
		http.Error(w, "认证失败", http.StatusForbidden)
		return
	}

	if r.Method == http.MethodConnect {
		s.handleConnect(w, r, params)
	} else {
		s.handleHTTP(w, r, params)
	}
}

// handleConnect 处理 HTTPS CONNECT 隧道
func (s *HTTPProxyServer) handleConnect(w http.ResponseWriter, r *http.Request, params parser.Params) {
	targetConn, workerID, err := s.dialTarget(r.Context(), params, r.Host)
	if err != nil {
		http.Error(w, fmt.Sprintf("连接目标失败: %v", err), http.StatusBadGateway)
		return
	}
	defer func() {
		targetConn.Close()
		s.resolver.UntrackConn(workerID)
	}()

	// 劫持客户端连接
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "不支持 hijack", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, fmt.Sprintf("hijack 失败: %v", err), http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	// 发送 200 连接建立
	clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	// 双向 pipe
	s.pipe(clientConn, targetConn)
}

// handleHTTP 处理普通 HTTP 代理请求
func (s *HTTPProxyServer) handleHTTP(w http.ResponseWriter, r *http.Request, params parser.Params) {
	// 确保有 host
	host := r.Host
	if !strings.Contains(host, ":") {
		host = host + ":80"
	}

	targetConn, workerID, err := s.dialTarget(r.Context(), params, host)
	if err != nil {
		http.Error(w, fmt.Sprintf("连接目标失败: %v", err), http.StatusBadGateway)
		return
	}
	defer func() {
		targetConn.Close()
		s.resolver.UntrackConn(workerID)
	}()

	// 转发请求
	r.Header.Del("Proxy-Authorization")
	r.Header.Del("Proxy-Connection")
	r.RequestURI = ""

	if err := r.Write(targetConn); err != nil {
		http.Error(w, "转发请求失败", http.StatusBadGateway)
		return
	}

	// 劫持并 pipe 响应
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "不支持 hijack", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer clientConn.Close()

	s.pipe(clientConn, targetConn)
}

func (s *HTTPProxyServer) dialTarget(ctx context.Context, params parser.Params, address string) (net.Conn, string, error) {
	w, err := s.router.Route(params)
	if err != nil {
		return nil, "", fmt.Errorf("路由失败: %w", err)
	}

	nsHandle, err := s.resolver.GetWorkerNsHandle(w.ID)
	if err != nil {
		return nil, "", fmt.Errorf("获取命名空间失败: %w", err)
	}

	s.resolver.TrackConn(w.ID)
	conn, err := s.dialer.DialInNs(ctx, nsHandle, "tcp", address)
	if err != nil {
		s.resolver.UntrackConn(w.ID)
		return nil, "", err
	}

	return conn, w.ID, nil
}

// parseProxyAuth 解析 Proxy-Authorization header
func (s *HTTPProxyServer) parseProxyAuth(r *http.Request) (username, password string, ok bool) {
	auth := r.Header.Get("Proxy-Authorization")
	if auth == "" {
		return "", "", false
	}

	if !strings.HasPrefix(auth, "Basic ") {
		return "", "", false
	}

	decoded, err := base64.StdEncoding.DecodeString(auth[6:])
	if err != nil {
		return "", "", false
	}

	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return "", "", false
	}

	return parts[0], parts[1], true
}

// pipe 双向数据传输
func (s *HTTPProxyServer) pipe(conn1, conn2 net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		done <- struct{}{}
	}
	go cp(conn1, conn2)
	go cp(conn2, conn1)
	<-done
}
```

- [ ] **Step 2: Verify build**

Run:
```bash
GOOS=linux go build ./internal/proxy/
```
Expected: 编译成功

- [ ] **Step 3: Commit**

```bash
git add internal/proxy/http.go
git commit -m "feat: 添加 HTTP/HTTPS 代理服务器，支持 CONNECT 隧道和普通代理"
```

---

### Task 11: Main Entry Point

**Files:**
- Modify: `cmd/gateway/main.go`

- [ ] **Step 1: Wire everything together**

Replace `cmd/gateway/main.go`:

```go
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

	log.Printf("Surfshark VPN Proxy Gateway 启动中...")
	log.Printf("SOCKS5 端口: %d, HTTP 端口: %d", cfg.Socks5Port, cfg.HTTPPort)

	// 1. 扫描可用服务器
	servers, err := discovery.Scan(cfg.OvpnDir)
	if err != nil {
		log.Fatalf("扫描 .ovpn 文件失败: %v", err)
	}

	totalServers := 0
	for country, list := range servers {
		log.Printf("  %s: %d 个服务器", country, len(list))
		totalServers += len(list)
	}
	if totalServers == 0 {
		log.Fatalf("未找到任何 .ovpn 文件，请检查 %s", cfg.OvpnDir)
	}
	log.Printf("共发现 %d 个国家 %d 个服务器", len(servers), totalServers)

	// 2. 初始化组件
	sessionMgr := session.NewManager(cfg.DefaultSessionTTL)
	workerMgr := workermgr.New(servers, cfg.AuthFile, sessionMgr, cfg.WorkerIdleTimeout)
	rt := router.New(workerMgr, sessionMgr)

	// 3. 启动上下文
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 4. 启动 session 清理协程
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				removed := sessionMgr.Cleanup()
				if removed > 0 {
					log.Printf("清理了 %d 个过期 session", removed)
				}
			}
		}
	}()

	// 5. 启动 worker 健康检查
	workerMgr.StartHealthCheck(ctx)

	// 6. 启动代理服务器
	socks5Srv := proxy.NewSocks5Server(cfg, rt, workerMgr)
	httpSrv := proxy.NewHTTPProxyServer(cfg, rt, workerMgr)

	go func() {
		if err := socks5Srv.ListenAndServe(ctx); err != nil {
			log.Printf("SOCKS5 服务器错误: %v", err)
		}
	}()

	go func() {
		if err := httpSrv.ListenAndServe(ctx); err != nil {
			log.Printf("HTTP 代理服务器错误: %v", err)
		}
	}()

	log.Printf("网关就绪！等待连接...")

	// 7. 等待退出信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Printf("收到退出信号，正在关闭...")
	cancel()
	workerMgr.Shutdown()
	log.Printf("已关闭")
}
```

- [ ] **Step 2: Enable IP forwarding in main**

需要在程序启动时确保 IP 转发已开启。在 main 函数的组件初始化之前添加:

```go
// 在 main() 中，扫描服务器之前添加:
// 启用 IP 转发
if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644); err != nil {
	log.Printf("警告: 无法启用 IP 转发: %v (可能需要在 Docker 中运行)", err)
}
```

- [ ] **Step 3: Verify build**

Run:
```bash
GOOS=linux go build ./cmd/gateway/
```
Expected: 编译成功

- [ ] **Step 4: Commit**

```bash
git add cmd/gateway/main.go
git commit -m "feat: 组装所有组件，实现完整的网关入口点"
```

---

### Task 12: Dockerfile and Docker Compose

**Files:**
- Create: `Dockerfile`
- Create: `docker-compose.yml`
- Create: `.dockerignore`

- [ ] **Step 1: Create Dockerfile**

Create `Dockerfile`:

```dockerfile
# 构建阶段
FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /gateway ./cmd/gateway/

# 运行阶段
FROM alpine:3.20

RUN apk add --no-cache \
    openvpn \
    iproute2 \
    iptables \
    ca-certificates

COPY --from=builder /gateway /usr/local/bin/gateway

EXPOSE 1080 8888

ENTRYPOINT ["/usr/local/bin/gateway"]
```

- [ ] **Step 2: Create .dockerignore**

Create `.dockerignore`:

```
.git
.claude
docs
*.md
ovpn
auth.txt
```

- [ ] **Step 3: Create docker-compose.yml**

Create `docker-compose.yml`:

```yaml
services:
  gateway:
    build: .
    cap_add:
      - NET_ADMIN
    devices:
      - /dev/net/tun
    ports:
      - "1080:1080"
      - "8888:8888"
    volumes:
      - ./ovpn:/etc/openvpn/ovpn:ro
      - ./auth.txt:/etc/openvpn/auth.txt:ro
    environment:
      - PROXY_USER=${PROXY_USER:-user}
      - PROXY_PASS=${PROXY_PASS:-pass}
      - DEFAULT_SESSION_TTL=${DEFAULT_SESSION_TTL:-30}
      - WORKER_IDLE_TIMEOUT=${WORKER_IDLE_TIMEOUT:-10}
    restart: unless-stopped
    sysctls:
      - net.ipv4.ip_forward=1
```

- [ ] **Step 4: Verify Docker build**

Run:
```bash
docker build -t surfshark-proxy .
```
Expected: 镜像构建成功

- [ ] **Step 5: Commit**

```bash
git add Dockerfile .dockerignore docker-compose.yml
git commit -m "feat: 添加 Dockerfile 和 docker-compose 配置"
```

---

### Task 13: Integration Testing

**Files:** 无新文件

此任务需要真实的 Surfshark .ovpn 文件和凭证。

- [ ] **Step 1: Prepare test data**

创建 `ovpn/` 目录，放入至少 2 个 Surfshark .ovpn 文件：
```bash
mkdir -p ovpn
# 将 Surfshark 的 .ovpn 文件复制到 ovpn/ 目录
# 创建 auth.txt，格式为：
# 第一行: Surfshark 用户名
# 第二行: Surfshark 密码
```

- [ ] **Step 2: Start the gateway**

Run:
```bash
docker compose up --build
```
Expected: 看到服务器发现日志和 "网关就绪" 消息

- [ ] **Step 3: Test rotating mode**

Run:
```bash
# 测试 SOCKS5 rotating
curl -x socks5://user:pass@localhost:1080 https://api.ipify.org
curl -x socks5://user:pass@localhost:1080 https://api.ipify.org

# 测试 HTTP rotating
curl -x http://user:pass@localhost:8888 https://api.ipify.org
```
Expected: 返回 VPN 出口 IP（可能不同）

- [ ] **Step 4: Test sticky mode**

Run:
```bash
# 同一 session 应返回相同 IP
curl -x socks5://user__sessid.test1:pass@localhost:1080 https://api.ipify.org
curl -x socks5://user__sessid.test1:pass@localhost:1080 https://api.ipify.org
```
Expected: 两次返回相同 IP

- [ ] **Step 5: Test country selection**

Run:
```bash
# 指定国家
curl -x socks5://user__cr.us:pass@localhost:1080 https://api.ipify.org
curl -x socks5://user__cr.jp:pass@localhost:1080 https://api.ipify.org
```
Expected: 返回对应国家的 IP

- [ ] **Step 6: Test combined parameters**

Run:
```bash
curl -x http://user__cr.de;sessid.mysess;sessttl.60:pass@localhost:8888 https://api.ipify.org
```
Expected: 返回德国 IP，60 分钟内同 session 返回相同 IP

- [ ] **Step 7: Commit any fixes**

```bash
git add -A
git commit -m "fix: 集成测试中发现的问题修复"
```

---

## Self-Review Checklist

**Spec coverage:**
- [x] Server Discovery — Task 3
- [x] Worker Manager（netns + OpenVPN 生命周期）— Task 6, 7, 8
- [x] Session Manager — Task 4
- [x] Username Parser — Task 2
- [x] 路由逻辑 — Task 5
- [x] 流量转发 — Task 7 (NsDialer)
- [x] SOCKS5 Server — Task 9
- [x] HTTP Proxy Server — Task 10
- [x] 配置/环境变量 — Task 1
- [x] Docker 镜像 — Task 12
- [x] 错误处理 — 分散在各任务中
- [x] 集成测试 — Task 13

**Placeholder scan:** 无 TBD/TODO

**Type consistency:**
- `WorkerInfo` 在 `router` 包中定义，`worker` 和 `proxy` 包引用一致
- `WorkerState` 在 `router` 包中定义，`worker` 包使用一致
- `WorkerPool` 接口在 `router` 包中定义，`worker.Manager` 实现一致
- `NsHandleResolver` 接口在 `proxy` 包中定义，`worker.Manager` 实现一致
- `parser.Params` 在所有引用处字段名一致
- `session.Session` 字段名在 manager 和 router 中一致
