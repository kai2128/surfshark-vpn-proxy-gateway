package router

import (
	"fmt"
	"sort"
	"sync/atomic"

	"surfshark-proxy/internal/parser"
	"surfshark-proxy/internal/session"
)

// WorkerState 表示 worker 当前生命周期状态。
type WorkerState int

const (
	WorkerCreating WorkerState = iota
	WorkerReady
	WorkerIdle
	WorkerClosing
	WorkerAwaitingReplacement
)

// WorkerInfo 是路由器依赖的最小 worker 视图。
type WorkerInfo struct {
	ID      string
	Country string
	State   WorkerState
}

// WorkerPool 提供 worker 查询与按需创建能力。
type WorkerPool interface {
	GetReadyWorkers(country string) []*WorkerInfo
	GetRoutableWorkers(country string) []*WorkerInfo
	RequestWorker(country string) (*WorkerInfo, error)
}

// Router 决定一个请求应该落到哪个 worker。
type Router struct {
	pool       WorkerPool
	session    *session.Manager
	roundRobin atomic.Uint64
}

// New 创建路由器。
func New(pool WorkerPool, sessionManager *session.Manager) *Router {
	return &Router{
		pool:    pool,
		session: sessionManager,
	}
}

// Route 按请求参数选择 worker。
func (r *Router) Route(params parser.Params) (*WorkerInfo, error) {
	if params.IsSticky() {
		return r.routeSticky(params)
	}

	return r.selectOrCreate(params.Country)
}

func (r *Router) routeSticky(params parser.Params) (*WorkerInfo, error) {
	if snapshot, ok := r.session.Lookup(params.SessionID); ok {
		if worker := r.findReady(snapshot.WorkerID); worker != nil {
			if params.Country == "" || worker.Country == params.Country {
				return worker, nil
			}
		}

		r.session.Remove(params.SessionID)
	}

	worker, err := r.selectOrCreate(params.Country)
	if err != nil {
		return nil, err
	}

	r.session.Bind(params.SessionID, worker.Country, params.SessionTTL, worker.ID)
	return worker, nil
}

func (r *Router) findReady(workerID string) *WorkerInfo {
	for _, worker := range r.pool.GetReadyWorkers("") {
		if worker.ID == workerID {
			return worker
		}
	}

	return nil
}

func (r *Router) selectOrCreate(country string) (*WorkerInfo, error) {
	workers := r.pool.GetRoutableWorkers(country)
	if len(workers) > 0 {
		sort.Slice(workers, func(i, j int) bool {
			return workers[i].ID < workers[j].ID
		})

		index := r.roundRobin.Add(1) - 1
		return workers[index%uint64(len(workers))], nil
	}

	worker, err := r.pool.RequestWorker(country)
	if err != nil {
		return nil, fmt.Errorf("request worker for country %q: %w", country, err)
	}

	return worker, nil
}
