package session

import (
	"sync"
	"time"
)

// Snapshot 是 session 的只读视图。
type Snapshot struct {
	ID        string
	WorkerID  string
	Country   string
	TTL       time.Duration
	CreatedAt time.Time
	LastUsed  time.Time
}

type stickySession struct {
	id        string
	workerID  string
	country   string
	ttl       time.Duration
	createdAt time.Time
	lastUsed  time.Time
}

func (s *stickySession) expired(now time.Time) bool {
	return now.Sub(s.createdAt) > s.ttl
}

func (s *stickySession) snapshot() Snapshot {
	return Snapshot{
		ID:        s.id,
		WorkerID:  s.workerID,
		Country:   s.country,
		TTL:       s.ttl,
		CreatedAt: s.createdAt,
		LastUsed:  s.lastUsed,
	}
}

// Manager 管理 sticky session 的绑定关系。
type Manager struct {
	mu         sync.RWMutex
	sessions   map[string]*stickySession
	defaultTTL time.Duration
}

// NewManager 创建 session 管理器。
func NewManager(defaultTTL time.Duration) *Manager {
	return &Manager{
		sessions:   make(map[string]*stickySession),
		defaultTTL: defaultTTL,
	}
}

// Lookup 查找未过期 session，并刷新最近使用时间。
func (m *Manager) Lookup(id string) (Snapshot, bool) {
	now := time.Now()

	m.mu.Lock()
	defer m.mu.Unlock()

	current, ok := m.sessions[id]
	if !ok {
		return Snapshot{}, false
	}
	if current.expired(now) {
		delete(m.sessions, id)
		return Snapshot{}, false
	}

	current.lastUsed = now
	return current.snapshot(), true
}

// Bind 绑定 session 到指定 worker。
func (m *Manager) Bind(id, country string, ttl time.Duration, workerID string) (Snapshot, bool) {
	if ttl <= 0 {
		ttl = m.defaultTTL
	}

	now := time.Now()

	m.mu.Lock()
	defer m.mu.Unlock()

	if current, ok := m.sessions[id]; ok && !current.expired(now) {
		current.workerID = workerID
		if country != "" {
			current.country = country
		}
		current.ttl = ttl
		current.lastUsed = now
		return current.snapshot(), false
	}

	current := &stickySession{
		id:        id,
		workerID:  workerID,
		country:   country,
		ttl:       ttl,
		createdAt: now,
		lastUsed:  now,
	}
	m.sessions[id] = current
	return current.snapshot(), true
}

// Remove 删除一个指定的 session。
func (m *Manager) Remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
}

// RemoveByWorker 删除指向某个 worker 的所有 session。
func (m *Manager) RemoveByWorker(workerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, current := range m.sessions {
		if current.workerID == workerID {
			delete(m.sessions, id)
		}
	}
}

// Cleanup 清理所有已过期 session。
func (m *Manager) Cleanup() int {
	now := time.Now()

	m.mu.Lock()
	defer m.mu.Unlock()

	removed := 0
	for id, current := range m.sessions {
		if current.expired(now) {
			delete(m.sessions, id)
			removed++
		}
	}

	return removed
}

// ActiveSessionsForWorker 返回仍然活跃的 session 数量。
func (m *Manager) ActiveSessionsForWorker(workerID string) int {
	now := time.Now()

	m.mu.RLock()
	defer m.mu.RUnlock()

	count := 0
	for _, current := range m.sessions {
		if current.workerID == workerID && !current.expired(now) {
			count++
		}
	}

	return count
}
