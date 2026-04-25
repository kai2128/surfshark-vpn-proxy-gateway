//go:build !linux

package worker

import (
	"context"
	"errors"
	"time"

	vishnetns "github.com/vishvananda/netns"
	"surfshark-proxy/internal/discovery"
	"surfshark-proxy/internal/router"
	"surfshark-proxy/internal/session"
)

// Manager 是非 Linux 环境下的占位实现。
type Manager struct{}

// New 返回一个不可用的 stub manager。
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
	_ = servers
	_ = authFile
	_ = sessionMgr
	_ = idleTimeout
	_ = maxLifetime
	_ = minPoolSize
	_ = verbose
	_ = preconnectConcurrency
	_ = lifetimeJitterPct
	_ = rotationGrace
	return &Manager{}
}

// GetReadyWorkers 在非 Linux 环境下返回空列表。
func (m *Manager) GetReadyWorkers(country string) []*router.WorkerInfo {
	_ = m
	_ = country
	return nil
}

// GetRoutableWorkers 在非 Linux 环境下返回空列表。
func (m *Manager) GetRoutableWorkers(country string) []*router.WorkerInfo {
	_ = m
	_ = country
	return nil
}

// RequestWorker 在非 Linux 环境下不可用。
func (m *Manager) RequestWorker(country string) (*router.WorkerInfo, error) {
	_ = m
	_ = country
	return nil, errors.New("worker manager requires linux")
}

// GetWorkerNsHandle 在非 Linux 环境下不可用。
func (m *Manager) GetWorkerNsHandle(workerID string) (vishnetns.NsHandle, error) {
	_ = m
	_ = workerID
	return vishnetns.None(), errors.New("worker manager requires linux")
}

// TrackConn 在非 Linux 环境下为空操作。
func (m *Manager) TrackConn(workerID string) {
	_ = m
	_ = workerID
}

// UntrackConn 在非 Linux 环境下为空操作。
func (m *Manager) UntrackConn(workerID string) {
	_ = m
	_ = workerID
}

// StartHealthCheck 在非 Linux 环境下为空操作。
func (m *Manager) StartHealthCheck(ctx context.Context) {
	_ = m
	_ = ctx
}

// StartPoolWarmer 在非 Linux 环境下为空操作。
func (m *Manager) StartPoolWarmer(ctx context.Context) {
	_ = m
	_ = ctx
}

// StartPreconnectCoordinator 在非 Linux 环境下为空操作。
func (m *Manager) StartPreconnectCoordinator(ctx context.Context) {
	_ = m
	_ = ctx
}

// Shutdown 在非 Linux 环境下为空操作。
func (m *Manager) Shutdown() {
	_ = m
}
