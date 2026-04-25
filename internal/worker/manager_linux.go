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
	createMaxAttempts   = 3
	maxNamespaceIndex   = 254
	maxWorkerSlots      = maxNamespaceIndex + 1
	reservedForOnDemand = 5
)

// Manager 负责 worker 的创建、查询、回收与关闭。
type Manager struct {
	mu          sync.RWMutex
	workers     map[string]*Worker
	usedIndexes map[int]bool
	servers     map[string][]discovery.Server
	authFile    string
	sessionMgr  *session.Manager
	idleTimeout time.Duration
	maxLifetime time.Duration
	minPoolSize int
	verbose     bool

	createGroup singleflight.Group
	wakeup      chan struct{}
	grow        chan struct{}

	rootCtx    context.Context
	rootCancel context.CancelFunc
	createWg   sync.WaitGroup

	preconnectSignal      chan struct{}
	preconnectSem         chan struct{}
	preconnectInflight    map[*Worker]struct{}
	preconnectConcurrency int
	rotationGrace         time.Duration
	lifetimeJitterPct     int

	// preconnectDispatch 允许测试替换调度目标。
	preconnectDispatch func(context.Context, *Worker)
	// preconnectCreate 允许测试注入 staged create 结果。
	preconnectCreate func(context.Context, string) (*Worker, error)
}

// New 创建 worker 管理器。
func New(
	servers map[string][]discovery.Server,
	authFile string,
	sessionMgr *session.Manager,
	idleTimeout, maxLifetime time.Duration,
	minPoolSize int,
	verbose bool,
	preconnectConcurrency int,
	lifetimeJitterPct int,
	rotationGrace time.Duration,
) *Manager {
	if minPoolSize < 0 {
		minPoolSize = 0
	}
	if minPoolSize >= maxWorkerSlots {
		panic(fmt.Sprintf("MIN_POOL_SIZE=%d 超过 namespace 索引槽位上限 %d", minPoolSize, maxWorkerSlots))
	}
	if minPoolSize*2+reservedForOnDemand > maxWorkerSlots {
		panic(fmt.Sprintf(
			"MIN_POOL_SIZE=%d 导致 rotation footprint %d 超过槽位预算 %d (2*MIN_POOL_SIZE + %d 保留)",
			minPoolSize, minPoolSize*2+reservedForOnDemand, maxWorkerSlots, reservedForOnDemand,
		))
	}
	if preconnectConcurrency <= 0 {
		preconnectConcurrency = 1
	}
	if lifetimeJitterPct < 0 {
		lifetimeJitterPct = 0
	} else if lifetimeJitterPct > 50 {
		lifetimeJitterPct = 50
	}

	rootCtx, rootCancel := context.WithCancel(context.Background())
	return &Manager{
		workers:               make(map[string]*Worker),
		usedIndexes:           make(map[int]bool),
		servers:               cloneServers(servers),
		authFile:              authFile,
		sessionMgr:            sessionMgr,
		idleTimeout:           idleTimeout,
		maxLifetime:           maxLifetime,
		minPoolSize:           minPoolSize,
		verbose:               verbose,
		wakeup:                make(chan struct{}, 1),
		grow:                  make(chan struct{}, 1),
		rootCtx:               rootCtx,
		rootCancel:            rootCancel,
		preconnectSignal:      make(chan struct{}, 1),
		preconnectSem:         make(chan struct{}, preconnectConcurrency),
		preconnectInflight:    make(map[*Worker]struct{}),
		preconnectConcurrency: preconnectConcurrency,
		rotationGrace:         rotationGrace,
		lifetimeJitterPct:     lifetimeJitterPct,
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

// signalPreconnect 非阻塞地唤醒 preconnect 协调器。
func (m *Manager) signalPreconnect() {
	select {
	case m.preconnectSignal <- struct{}{}:
	default:
	}
}

// managedContext 让后台协程同时受调用方 ctx 和 manager rootCtx 驱动。
func (m *Manager) managedContext(parent context.Context) context.Context {
	if parent == nil {
		parent = context.Background()
	}

	ctx, cancel := context.WithCancel(parent)
	go func() {
		select {
		case <-m.rootCtx.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx
}

// countReady 返回可服务池的大小。
func (m *Manager) countReady() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	count := 0
	for _, worker := range m.workers {
		switch worker.Info().State {
		case router.WorkerReady, router.WorkerIdle, router.WorkerAwaitingReplacement:
			count++
		}
	}
	return count
}

// acquireIndex 分配最小可用的 netns 索引。
// 调用方必须持有 m.mu 写锁。
func (m *Manager) acquireIndex() int {
	for candidate := 0; candidate <= maxNamespaceIndex; candidate++ {
		if !m.usedIndexes[candidate] {
			m.usedIndexes[candidate] = true
			return candidate
		}
	}
	return -1
}

// releaseIndex 归还 netns 索引。
// 调用方必须持有 m.mu 写锁。
func (m *Manager) releaseIndex(index int) {
	delete(m.usedIndexes, index)
}

// GetReadyWorkers 返回完整 serving 集合，sticky 路径可见 AwaitingReplacement。
func (m *Manager) GetReadyWorkers(country string) []*router.WorkerInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var ready []*router.WorkerInfo
	for _, worker := range m.workers {
		info := worker.Info()
		switch info.State {
		case router.WorkerReady, router.WorkerIdle, router.WorkerAwaitingReplacement:
		default:
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

// GetRoutableWorkers 返回新流量优先应使用的 worker 集合。
func (m *Manager) GetRoutableWorkers(country string) []*router.WorkerInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var active []*router.WorkerInfo
	var awaiting []*router.WorkerInfo

	for _, worker := range m.workers {
		info := worker.Info()
		if country != "" && info.Country != country {
			continue
		}
		switch info.State {
		case router.WorkerReady, router.WorkerIdle:
			active = append(active, info)
		case router.WorkerAwaitingReplacement:
			awaiting = append(awaiting, info)
		}
	}

	result := active
	if len(result) == 0 {
		result = awaiting
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].ID < result[j].ID
	})
	return result
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
		return m.createForCountry(m.rootCtx, country)
	})
	if err != nil {
		return nil, err
	}

	return value.(*router.WorkerInfo), nil
}

func (m *Manager) pickReady(country string) *router.WorkerInfo {
	routable := m.GetRoutableWorkers(country)
	if len(routable) == 0 {
		return nil
	}
	return routable[0]
}

func (m *Manager) createForCountry(ctx context.Context, country string) (*router.WorkerInfo, error) {
	worker, err := m.tryCreate(ctx, country)
	if err != nil {
		return nil, err
	}

	if !m.publishWorker(worker) {
		return nil, m.rootCtx.Err()
	}
	return worker.Info(), nil
}

// createForCountryStaged 创建并返回未发布的 worker。
func (m *Manager) createForCountryStaged(ctx context.Context, country string) (*Worker, error) {
	return m.tryCreate(ctx, country)
}

func (m *Manager) tryCreate(ctx context.Context, country string) (*Worker, error) {
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
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		m.mu.Lock()
		namespaceIndex := m.acquireIndex()
		m.mu.Unlock()
		if namespaceIndex < 0 {
			return nil, fmt.Errorf("namespace index pool exhausted (max %d)", maxNamespaceIndex+1)
		}

		server := candidates[(namespaceIndex+attempt)%len(candidates)]
		worker, err := m.createWorker(ctx, server, namespaceIndex)
		if err == nil {
			return worker, nil
		}

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

// TrackConn 记录连接进入。
func (m *Manager) TrackConn(workerID string) {
	m.mu.RLock()
	worker := m.workers[workerID]
	m.mu.RUnlock()

	if worker != nil {
		worker.IncrConns()
	}
}

// UntrackConn 记录连接结束。
func (m *Manager) UntrackConn(workerID string) {
	m.mu.RLock()
	worker := m.workers[workerID]
	m.mu.RUnlock()

	if worker != nil {
		worker.DecrConns()
	}
}

func (m *Manager) createWorker(ctx context.Context, server discovery.Server, index int) (*Worker, error) {
	m.createWg.Add(1)
	defer m.createWg.Done()

	namespaceName := fmt.Sprintf("worker-%d", index)
	namespace, err := nsmanager.Create(namespaceName, index)
	if err != nil {
		return nil, fmt.Errorf("create namespace %s: %w", namespaceName, err)
	}

	if err := ctx.Err(); err != nil {
		_ = namespace.Destroy()
		return nil, fmt.Errorf("create cancelled before start: %w", err)
	}

	verb := "1"
	if m.verbose {
		verb = "3"
	}

	cmd := exec.Command(
		"ip", "netns", "exec", namespaceName,
		"openvpn",
		"--config", server.OvpnPath,
		"--auth-user-pass", m.authFile,
		"--auth-nocache",
		"--verb", verb,
		"--connect-retry", "3",
		"--connect-timeout", "30",
	)

	if m.verbose {
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

		go pumpLog(namespaceName, "stdout", stdout)
		go pumpLog(namespaceName, "stderr", stderr)
		diagnoseNetns(namespaceName)
	} else {
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Start(); err != nil {
			_ = namespace.Destroy()
			return nil, fmt.Errorf("start openvpn: %w", err)
		}
	}

	worker := &Worker{
		ID:                   namespaceName,
		Index:                index,
		Server:               server,
		State:                router.WorkerCreating,
		Namespace:            namespace,
		OvpnProcess:          cmd,
		CreatedAt:            time.Now(),
		LastUsed:             time.Now(),
		effectiveMaxLifetime: computeEffectiveMaxLifetime(m.maxLifetime, m.lifetimeJitterPct),
		processDone:          make(chan struct{}),
	}

	go func() {
		_ = cmd.Wait()
		worker.processExited.Store(true)
		close(worker.processDone)
		m.signalWakeup()
	}()

	if err := ctx.Err(); err != nil {
		_ = worker.Stop()
		return nil, fmt.Errorf("create cancelled after start: %w", err)
	}

	if err := m.waitForTun(ctx, namespaceName, 30*time.Second); err != nil {
		_ = worker.Stop()
		return nil, fmt.Errorf("wait for tun device: %w", err)
	}

	if err := ctx.Err(); err != nil {
		_ = worker.Stop()
		return nil, fmt.Errorf("create cancelled after tun ready: %w", err)
	}

	worker.markReady()
	return worker, nil
}

// publishWorker 把已构建完成的 worker 发布到池中。
func (m *Manager) publishWorker(worker *Worker) bool {
	m.mu.Lock()
	if err := m.rootCtx.Err(); err != nil {
		m.releaseIndex(worker.Index)
		m.mu.Unlock()
		_ = worker.Stop()
		log.Printf("worker %s 创建完成后丢弃：Shutdown 已触发 (%v)", worker.ID, err)
		return false
	}

	m.workers[worker.ID] = worker
	count := len(m.workers)
	m.mu.Unlock()

	log.Printf("worker %s 已创建 [%s/%s]，当前共 %d 个 worker", worker.ID, worker.Server.Country, worker.Server.Name, count)
	return true
}

func (m *Manager) waitForTun(parent context.Context, namespaceName string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("namespace %s wait cancelled: %w", namespaceName, ctx.Err())
		case <-ticker.C:
			cmd := exec.CommandContext(ctx, "ip", "netns", "exec", namespaceName, "ip", "link", "show", "type", "tun")
			output, err := cmd.Output()
			if err == nil && len(output) > 0 {
				return nil
			}
		}
	}
}

// StartHealthCheck 启动后台巡检。
func (m *Manager) StartHealthCheck(ctx context.Context) {
	ctx = m.managedContext(ctx)

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
		reason string
	}

	var victims []victim
	awaitingScheduled := 0

	m.mu.Lock()
	for id, worker := range m.workers {
		if worker.ProcessExited() {
			victims = append(victims, victim{id: id, worker: worker, reason: "进程退出"})
			delete(m.workers, id)
			m.releaseIndex(worker.Index)
			continue
		}

		if m.maxLifetime > 0 {
			lifetime := worker.effectiveLifetime()
			if lifetime == 0 {
				lifetime = m.maxLifetime
			}
			if lifetime > 0 && worker.Age() > lifetime {
				if worker.markAwaitingReplacement() {
					awaitingScheduled++
					log.Printf("worker %s 到期，等待替换（有效寿命 %d 分钟）", id, int(lifetime.Round(time.Minute)/time.Minute))
				}
			}
		}

		if worker.isClosingDrained() {
			victims = append(victims, victim{id: id, worker: worker, reason: "达到最大寿命"})
			delete(m.workers, id)
			m.releaseIndex(worker.Index)
			continue
		}

		if !worker.isAwaitingReplacement() &&
			worker.IsIdle() &&
			m.sessionMgr != nil &&
			m.sessionMgr.ActiveSessionsForWorker(id) == 0 &&
			time.Since(worker.LastUsedAt()) > m.idleTimeout {
			victims = append(victims, victim{id: id, worker: worker, reason: "空闲超时"})
			delete(m.workers, id)
			m.releaseIndex(worker.Index)
		}
	}
	remaining := len(m.workers)
	m.mu.Unlock()

	for _, victim := range victims {
		_ = victim.worker.Stop()
		if m.sessionMgr != nil {
			m.sessionMgr.RemoveByWorker(victim.id)
		}
		log.Printf("worker %s 已销毁 (%s)，当前共 %d 个 worker", victim.id, victim.reason, remaining)
	}

	if len(victims) > 0 {
		m.signalGrow()
		m.signalPreconnect()
	}
	if awaitingScheduled > 0 {
		m.signalPreconnect()
	}
}

// StartPoolWarmer 启动池扩容协程，按需把池补齐到 minPoolSize。
func (m *Manager) StartPoolWarmer(ctx context.Context) {
	if m.minPoolSize <= 0 {
		return
	}
	ctx = m.managedContext(ctx)

	m.signalGrow()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-m.grow:
			}

			for ctx.Err() == nil {
				if m.countReady() >= m.minPoolSize {
					break
				}

				if _, err := m.createForCountry(ctx, ""); err != nil {
					log.Printf("池扩容失败（当前 %d/%d）: %v", m.countReady(), m.minPoolSize, err)
					break
				}
			}
		}
	}()
}

// Shutdown 停止所有 worker，并阻止新的创建发布回池。
func (m *Manager) Shutdown() {
	if m.rootCancel != nil {
		m.rootCancel()
	}
	m.createWg.Wait()

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

// pumpLog 从 reader 持续读取行并转发到网关日志。
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
