//go:build linux

package worker

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	vishnetns "github.com/vishvananda/netns"
	"golang.org/x/sync/singleflight"
	"surfshark-proxy/internal/discovery"
	nsmanager "surfshark-proxy/internal/netns"
	"surfshark-proxy/internal/router"
	"surfshark-proxy/internal/session"
)

const createMaxAttempts = 3

// Manager 负责 worker 的创建、查询、回收与关闭。
type Manager struct {
	mu           sync.RWMutex
	workers      map[string]*Worker
	servers      map[string][]discovery.Server
	authFile     string
	sessionMgr   *session.Manager
	idleTimeout  time.Duration
	indexCounter atomic.Int32
	createGroup  singleflight.Group
}

// New 创建 worker 管理器。
func New(servers map[string][]discovery.Server, authFile string, sessionMgr *session.Manager, idleTimeout time.Duration) *Manager {
	return &Manager{
		workers:     make(map[string]*Worker),
		servers:     cloneServers(servers),
		authFile:    authFile,
		sessionMgr:  sessionMgr,
		idleTimeout: idleTimeout,
	}
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

	startOffset := int(m.indexCounter.Load())
	var lastErr error

	for attempt := 0; attempt < attempts; attempt++ {
		namespaceIndex := int(m.indexCounter.Add(1) - 1)
		server := candidates[(startOffset+attempt)%len(candidates)]

		worker, err := m.createWorker(server, namespaceIndex)
		if err == nil {
			return worker.Info(), nil
		}

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

	if err := cmd.Start(); err != nil {
		_ = namespace.Destroy()
		return nil, fmt.Errorf("start openvpn: %w", err)
	}

	worker := &Worker{
		ID:          namespaceName,
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
			continue
		}

		if worker.IsIdle() &&
			m.sessionMgr.ActiveSessionsForWorker(id) == 0 &&
			time.Since(worker.LastUsedAt()) > m.idleTimeout {
			victims = append(victims, victim{id: id, worker: worker})
			delete(m.workers, id)
		}
	}
	m.mu.Unlock()

	for _, victim := range victims {
		_ = victim.worker.Stop()
		m.sessionMgr.RemoveByWorker(victim.id)
	}
}

// Shutdown 停止所有 worker。
func (m *Manager) Shutdown() {
	m.mu.Lock()
	workers := make([]*Worker, 0, len(m.workers))
	for _, worker := range m.workers {
		workers = append(workers, worker)
	}
	m.workers = make(map[string]*Worker)
	m.mu.Unlock()

	for _, worker := range workers {
		_ = worker.Stop()
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
