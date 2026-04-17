//go:build linux

package worker

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sort"
	"sync"
	"time"

	vishnetns "github.com/vishvananda/netns"
	"golang.org/x/sync/singleflight"
	"surfshark-proxy/internal/discovery"
	nsmanager "surfshark-proxy/internal/netns"
	"surfshark-proxy/internal/router"
	"surfshark-proxy/internal/session"
)

const (
	createMaxAttempts = 3
	// maxNamespaceIndex 对应 netns IP 中的 byte(index) 取值范围。
	maxNamespaceIndex = 254
)

// Manager 负责 worker 的创建、查询、回收与关闭。
type Manager struct {
	mu          sync.RWMutex
	workers     map[string]*Worker
	usedIndexes map[int]bool // 当前被占用的 netns 索引
	servers     map[string][]discovery.Server
	authFile    string
	sessionMgr  *session.Manager
	idleTimeout time.Duration
	minPoolSize int
	createGroup singleflight.Group
	// wakeup 用于在 worker 进程退出时立刻触发健康检查，
	// 避免等待 30 秒 ticker。缓冲为 1：多次信号合并为一次检查。
	wakeup chan struct{}
	// grow 通知池扩容协程去补齐到 minPoolSize。
	// 缓冲为 1：多次信号合并为一次检查。
	grow chan struct{}
}

// New 创建 worker 管理器。
func New(servers map[string][]discovery.Server, authFile string, sessionMgr *session.Manager, idleTimeout time.Duration, minPoolSize int) *Manager {
	return &Manager{
		workers:     make(map[string]*Worker),
		usedIndexes: make(map[int]bool),
		servers:     cloneServers(servers),
		authFile:    authFile,
		sessionMgr:  sessionMgr,
		idleTimeout: idleTimeout,
		minPoolSize: minPoolSize,
		wakeup:      make(chan struct{}, 1),
		grow:        make(chan struct{}, 1),
	}
}

// signalWakeup 非阻塞地唤醒健康检查协程。
func (m *Manager) signalWakeup() {
	select {
	case m.wakeup <- struct{}{}:
	default:
	}
}

// signalGrow 非阻塞地唤醒池扩容协程。
func (m *Manager) signalGrow() {
	select {
	case m.grow <- struct{}{}:
	default:
	}
}

// countReady 返回处于 Ready/Idle 状态的 worker 数量。
func (m *Manager) countReady() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	count := 0
	for _, worker := range m.workers {
		info := worker.Info()
		if info.State == router.WorkerReady || info.State == router.WorkerIdle {
			count++
		}
	}
	return count
}

// acquireIndex 分配最小可用的 netns 索引，找不到返回 -1。
// 调用者必须持有 m.mu 的写锁。
func (m *Manager) acquireIndex() int {
	for candidate := 0; candidate <= maxNamespaceIndex; candidate++ {
		if !m.usedIndexes[candidate] {
			m.usedIndexes[candidate] = true
			return candidate
		}
	}
	return -1
}

// releaseIndex 归还索引供后续 worker 复用。
// 调用者必须持有 m.mu 的写锁。
func (m *Manager) releaseIndex(index int) {
	delete(m.usedIndexes, index)
}

// GetReadyWorkers 返回已就绪 worker 列表。
func (m *Manager) GetReadyWorkers(country string) []*router.WorkerInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var ready []*router.WorkerInfo
	for _, worker := range m.workers {
		info := worker.Info()
		if info.State != router.WorkerReady && info.State != router.WorkerIdle {
			continue
		}
		if country == "" || info.Country == country {
			ready = append(ready, info)
		}
	}

	sort.Slice(ready, func(i, j int) bool {
		return ready[i].ID < ready[j].ID
	})

	return ready
}

// RequestWorker 按国家同步创建一个 worker。
func (m *Manager) RequestWorker(country string) (*router.WorkerInfo, error) {
	key := country
	if key == "" {
		key = "__any__"
	}

	value, err, _ := m.createGroup.Do(key, func() (any, error) {
		if ready := m.pickReady(country); ready != nil {
			return ready, nil
		}
		return m.createForCountry(country)
	})
	if err != nil {
		return nil, err
	}

	return value.(*router.WorkerInfo), nil
}

func (m *Manager) pickReady(country string) *router.WorkerInfo {
	ready := m.GetReadyWorkers(country)
	if len(ready) == 0 {
		return nil
	}
	return ready[0]
}

func (m *Manager) createForCountry(country string) (*router.WorkerInfo, error) {
	candidates, err := m.serverCandidates(country)
	if err != nil {
		return nil, err
	}

	attempts := createMaxAttempts
	if len(candidates) < attempts {
		attempts = len(candidates)
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		m.mu.Lock()
		namespaceIndex := m.acquireIndex()
		m.mu.Unlock()

		if namespaceIndex < 0 {
			return nil, fmt.Errorf("namespace index pool exhausted (max %d)", maxNamespaceIndex+1)
		}

		server := candidates[(namespaceIndex+attempt)%len(candidates)]

		worker, err := m.createWorker(server, namespaceIndex)
		if err == nil {
			return worker.Info(), nil
		}

		// 创建失败：归还索引供后续尝试复用。
		m.mu.Lock()
		m.releaseIndex(namespaceIndex)
		m.mu.Unlock()

		lastErr = err
		log.Printf("创建 worker 失败，国家=%s，服务器=%s，尝试=%d/%d: %v", country, server.Name, attempt+1, attempts, err)
	}

	return nil, fmt.Errorf("unable to create worker for country %q: %w", country, lastErr)
}

func (m *Manager) serverCandidates(country string) ([]discovery.Server, error) {
	if country != "" {
		servers := append([]discovery.Server(nil), m.servers[country]...)
		if len(servers) == 0 {
			return nil, fmt.Errorf("no surfshark servers available for country %q", country)
		}
		return servers, nil
	}

	var servers []discovery.Server
	for _, group := range m.servers {
		servers = append(servers, group...)
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("no surfshark servers discovered")
	}

	sort.Slice(servers, func(i, j int) bool {
		if servers[i].Country == servers[j].Country {
			return servers[i].Name < servers[j].Name
		}
		return servers[i].Country < servers[j].Country
	})
	return servers, nil
}

// GetWorkerNsHandle 返回目标 worker 的命名空间句柄。
func (m *Manager) GetWorkerNsHandle(workerID string) (vishnetns.NsHandle, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	worker, ok := m.workers[workerID]
	if !ok {
		return vishnetns.None(), fmt.Errorf("worker %s not found", workerID)
	}

	return worker.NsHandle(), nil
}

// TrackConn 记录一次连接进入。
func (m *Manager) TrackConn(workerID string) {
	m.mu.RLock()
	worker := m.workers[workerID]
	m.mu.RUnlock()

	if worker != nil {
		worker.IncrConns()
	}
}

// UntrackConn 记录一次连接结束。
func (m *Manager) UntrackConn(workerID string) {
	m.mu.RLock()
	worker := m.workers[workerID]
	m.mu.RUnlock()

	if worker != nil {
		worker.DecrConns()
	}
}

func (m *Manager) createWorker(server discovery.Server, index int) (*Worker, error) {
	namespaceName := fmt.Sprintf("worker-%d", index)
	namespace, err := nsmanager.Create(namespaceName, index)
	if err != nil {
		return nil, fmt.Errorf("create namespace %s: %w", namespaceName, err)
	}

	cmd := exec.Command(
		"ip", "netns", "exec", namespaceName,
		"openvpn",
		"--config", server.OvpnPath,
		"--auth-user-pass", m.authFile,
		"--auth-nocache",
		"--verb", "3",
		"--connect-retry", "3",
		"--connect-timeout", "30",
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = namespace.Destroy()
		return nil, fmt.Errorf("pipe openvpn stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = namespace.Destroy()
		return nil, fmt.Errorf("pipe openvpn stderr: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = namespace.Destroy()
		return nil, fmt.Errorf("start openvpn: %w", err)
	}

	// 把 OpenVPN 的输出转到网关日志（加 worker ID 前缀），方便诊断。
	go pumpLog(namespaceName, "stdout", stdout)
	go pumpLog(namespaceName, "stderr", stderr)

	// 一次性打印 netns 的关键状态，定位 DNS/路由问题。
	diagnoseNetns(namespaceName)

	worker := &Worker{
		ID:          namespaceName,
		Index:       index,
		Server:      server,
		State:       router.WorkerCreating,
		Namespace:   namespace,
		OvpnProcess: cmd,
		CreatedAt:   time.Now(),
		LastUsed:    time.Now(),
		processDone: make(chan struct{}),
	}

	go func() {
		_ = cmd.Wait()
		worker.processExited.Store(true)
		close(worker.processDone)
		// 通知健康检查协程立即回收，而不是等下一次 ticker。
		m.signalWakeup()
	}()

	if err := m.waitForTun(namespaceName, 30*time.Second); err != nil {
		_ = worker.Stop()
		return nil, fmt.Errorf("wait for tun device: %w", err)
	}

	worker.markReady()

	m.mu.Lock()
	m.workers[worker.ID] = worker
	m.mu.Unlock()

	return worker, nil
}

func (m *Manager) waitForTun(namespaceName string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for tun in namespace %s", namespaceName)
		case <-ticker.C:
			cmd := exec.Command("ip", "netns", "exec", namespaceName, "ip", "link", "show", "type", "tun")
			output, err := cmd.Output()
			if err == nil && len(output) > 0 {
				return nil
			}
		}
	}
}

// StartHealthCheck 启动后台巡检。
// 在周期性 ticker 之外，还会响应 wakeup 信号（worker 进程退出时）立即清理。
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
			case <-m.wakeup:
				m.checkWorkers()
			}
		}
	}()
}

func (m *Manager) checkWorkers() {
	type victim struct {
		id     string
		worker *Worker
	}

	var victims []victim

	m.mu.Lock()
	for id, worker := range m.workers {
		if worker.ProcessExited() {
			victims = append(victims, victim{id: id, worker: worker})
			delete(m.workers, id)
			m.releaseIndex(worker.Index)
			continue
		}

		if worker.IsIdle() &&
			m.sessionMgr.ActiveSessionsForWorker(id) == 0 &&
			time.Since(worker.LastUsedAt()) > m.idleTimeout {
			victims = append(victims, victim{id: id, worker: worker})
			delete(m.workers, id)
			m.releaseIndex(worker.Index)
		}
	}
	m.mu.Unlock()

	for _, victim := range victims {
		_ = victim.worker.Stop()
		m.sessionMgr.RemoveByWorker(victim.id)
	}

	// 回收完 worker，池可能跌破 minPoolSize，让扩容协程补位。
	if len(victims) > 0 {
		m.signalGrow()
	}
}

// StartPoolWarmer 启动池扩容协程，按需创建 worker 把池补齐到 minPoolSize。
// 串行创建：同一时刻只创建一个 worker，避免瞬时资源压力。
func (m *Manager) StartPoolWarmer(ctx context.Context) {
	if m.minPoolSize <= 0 {
		return
	}

	// 启动时先触发一次，让池从 0 长到目标大小。
	m.signalGrow()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-m.grow:
			}

			// 循环补位：每次创建一个，直到达到目标或失败。
			for ctx.Err() == nil {
				if m.countReady() >= m.minPoolSize {
					break
				}

				if _, err := m.createForCountry(""); err != nil {
					log.Printf("池扩容失败（当前 %d/%d）: %v", m.countReady(), m.minPoolSize, err)
					break
				}
			}
		}
	}()
}

// Shutdown 停止所有 worker。
func (m *Manager) Shutdown() {
	m.mu.Lock()
	workers := make([]*Worker, 0, len(m.workers))
	for _, worker := range m.workers {
		workers = append(workers, worker)
		m.releaseIndex(worker.Index)
	}
	m.workers = make(map[string]*Worker)
	m.mu.Unlock()

	for _, worker := range workers {
		_ = worker.Stop()
	}
}

// diagnoseNetns 打印 netns 内的关键网络状态，便于排查 DNS/路由故障。
// 所有命令均为一次性执行，失败只记录日志，不影响调用方。
func diagnoseNetns(namespaceName string) {
	checks := []struct {
		label string
		args  []string
	}{
		{"resolv.conf", []string{"cat", "/etc/resolv.conf"}},
		{"ip addr", []string{"ip", "addr"}},
		{"ip route", []string{"ip", "route"}},
		{"ping 1.1.1.1", []string{"ping", "-c", "1", "-W", "3", "1.1.1.1"}},
	}

	for _, check := range checks {
		args := append([]string{"netns", "exec", namespaceName}, check.args...)
		cmd := exec.Command("ip", args...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("[diagnose %s] %s failed: %v: %s", namespaceName, check.label, err, string(output))
			continue
		}
		log.Printf("[diagnose %s] %s:\n%s", namespaceName, check.label, string(output))
	}
}

// pumpLog 从 reader 持续读取行并打印到网关日志，直至 reader 关闭（进程结束）。
func pumpLog(workerID, stream string, reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		log.Printf("[openvpn %s %s] %s", workerID, stream, scanner.Text())
	}
}

func cloneServers(servers map[string][]discovery.Server) map[string][]discovery.Server {
	cloned := make(map[string][]discovery.Server, len(servers))
	for country, group := range servers {
		cloned[country] = append([]discovery.Server(nil), group...)
		sort.Slice(cloned[country], func(i, j int) bool {
			return cloned[country][i].Name < cloned[country][j].Name
		})
	}
	return cloned
}
