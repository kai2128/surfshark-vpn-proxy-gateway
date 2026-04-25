//go:build linux

package worker

import (
	"context"
	"testing"
	"time"

	"surfshark-proxy/internal/router"
	"surfshark-proxy/internal/session"
)

func newTestManager() *Manager {
	rootCtx, rootCancel := context.WithCancel(context.Background())
	return &Manager{
		workers:            make(map[string]*Worker),
		usedIndexes:        make(map[int]bool),
		wakeup:             make(chan struct{}, 1),
		grow:               make(chan struct{}, 1),
		rootCtx:            rootCtx,
		rootCancel:         rootCancel,
		preconnectSignal:   make(chan struct{}, 1),
		preconnectSem:      make(chan struct{}, 2),
		preconnectInflight: make(map[*Worker]struct{}),
		rotationGrace:      5 * time.Minute,
	}
}

func putWorker(m *Manager, id string, state router.WorkerState) *Worker {
	w := &Worker{
		ID:          id,
		Index:       len(m.workers),
		State:       state,
		CreatedAt:   time.Now(),
		LastUsed:    time.Now(),
		processDone: make(chan struct{}),
	}
	m.workers[id] = w
	m.usedIndexes[w.Index] = true
	return w
}

func TestNewRejectsOversizedPool(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on oversized MinPoolSize")
		}
	}()

	_ = New(nil, "", nil, 0, 0, 200, false, 3, 10, 0)
}

func TestCountReadyIncludesAwaiting(t *testing.T) {
	m := newTestManager()
	putWorker(m, "r", router.WorkerReady)
	putWorker(m, "i", router.WorkerIdle)
	putWorker(m, "a", router.WorkerAwaitingReplacement)
	putWorker(m, "c", router.WorkerClosing)

	if got := m.countReady(); got != 3 {
		t.Fatalf("expected 3 (Ready+Idle+Awaiting), got %d", got)
	}
}

func TestGetReadyWorkersIncludesAwaiting(t *testing.T) {
	m := newTestManager()
	putWorker(m, "r", router.WorkerReady)
	putWorker(m, "a", router.WorkerAwaitingReplacement)
	putWorker(m, "c", router.WorkerClosing)

	got := m.GetReadyWorkers("")
	if len(got) != 2 {
		t.Fatalf("expected 2 entries (Ready+Awaiting), got %d", len(got))
	}
}

func TestGetRoutableWorkersPrefersReady(t *testing.T) {
	m := newTestManager()
	putWorker(m, "r", router.WorkerReady)
	putWorker(m, "a", router.WorkerAwaitingReplacement)

	got := m.GetRoutableWorkers("")
	if len(got) != 1 || got[0].ID != "r" {
		t.Fatalf("expected only Ready worker, got %+v", got)
	}
}

func TestGetRoutableWorkersFallsBackToAwaiting(t *testing.T) {
	m := newTestManager()
	putWorker(m, "a", router.WorkerAwaitingReplacement)

	got := m.GetRoutableWorkers("")
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("expected fallback to Awaiting, got %+v", got)
	}
}

func TestPickReadySkipsAwaitingWhenReadyExists(t *testing.T) {
	m := newTestManager()
	putWorker(m, "a-earliest", router.WorkerAwaitingReplacement)
	putWorker(m, "b-ready", router.WorkerReady)

	got := m.pickReady("")
	if got == nil || got.ID != "b-ready" {
		t.Fatalf("pickReady should pick Ready over Awaiting, got %+v", got)
	}
}

func TestCheckWorkersMarksExpiredAsAwaiting(t *testing.T) {
	m := newTestManager()
	m.maxLifetime = time.Millisecond

	w := putWorker(m, "w-old", router.WorkerReady)
	w.effectiveMaxLifetime = time.Millisecond
	w.CreatedAt = time.Now().Add(-time.Hour)

	m.checkWorkers()

	if !w.isAwaitingReplacement() {
		t.Fatalf("expected worker to be AwaitingReplacement, got %v", w.State)
	}
	select {
	case <-m.preconnectSignal:
	default:
		t.Fatal("expected signalPreconnect to have fired")
	}
}

func TestCheckWorkersIdleSkipsAwaiting(t *testing.T) {
	m := newTestManager()
	m.idleTimeout = time.Nanosecond
	m.sessionMgr = session.NewManager(time.Hour)

	w := putWorker(m, "w-await", router.WorkerAwaitingReplacement)
	w.LastUsed = time.Now().Add(-time.Hour)

	m.checkWorkers()

	if _, ok := m.workers["w-await"]; !ok {
		t.Fatal("AwaitingReplacement worker should not be removed by idle branch")
	}
}
