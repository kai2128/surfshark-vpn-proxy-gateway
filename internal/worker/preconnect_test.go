//go:build linux

package worker

import (
	"context"
	"testing"
	"time"

	"surfshark-proxy/internal/router"
)

func TestPreconnectScanDispatchesAwaiting(t *testing.T) {
	m := newTestManager()
	w := putWorker(m, "w0", router.WorkerAwaitingReplacement)
	w.rotationScheduledAt = time.Now()

	dispatched := make(chan *Worker, 1)
	m.preconnectDispatch = func(ctx context.Context, target *Worker) {
		dispatched <- target
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go m.runPreconnectCoordinator(ctx)
	m.signalPreconnect()

	select {
	case got := <-dispatched:
		if got != w {
			t.Fatalf("expected dispatch for w0, got %v", got.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("expected dispatch within 1s")
	}
}

func TestPreconnectCoordinatorCtxCancelledByRootCancel(t *testing.T) {
	m := newTestManager()
	m.maxLifetime = time.Minute
	w := putWorker(m, "w0", router.WorkerAwaitingReplacement)
	w.rotationScheduledAt = time.Now()

	done := make(chan struct{})
	m.preconnectDispatch = func(ctx context.Context, target *Worker) {
		<-ctx.Done()
		close(done)
	}

	m.StartPreconnectCoordinator(context.Background())
	m.signalPreconnect()
	m.rootCancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("expected rootCancel to cancel coordinator context")
	}
}

func TestPreconnectInflightDedup(t *testing.T) {
	m := newTestManager()
	w := putWorker(m, "w0", router.WorkerAwaitingReplacement)
	w.rotationScheduledAt = time.Now()

	count := 0
	m.preconnectDispatch = func(ctx context.Context, target *Worker) {
		count++
	}

	m.dispatchPending(context.Background())
	m.dispatchPending(context.Background())

	if count != 1 {
		t.Fatalf("expected 1 dispatch due to inflight dedup, got %d", count)
	}
}

func TestDispatchPendingAllowsInitialBurstUpToMinPoolSize(t *testing.T) {
	m := newTestManager()
	m.minPoolSize = 3

	w1 := putWorker(m, "a1", router.WorkerAwaitingReplacement)
	w1.rotationScheduledAt = time.Now()
	w2 := putWorker(m, "a2", router.WorkerAwaitingReplacement)
	w2.rotationScheduledAt = time.Now()
	w3 := putWorker(m, "a3", router.WorkerAwaitingReplacement)
	w3.rotationScheduledAt = time.Now()

	dispatched := 0
	m.preconnectDispatch = func(ctx context.Context, target *Worker) {
		dispatched++
	}

	m.dispatchPending(context.Background())

	if dispatched != 3 {
		t.Fatalf("expected initial dispatch burst to reach minPoolSize, got %d", dispatched)
	}
}

func TestDispatchFootprintGate(t *testing.T) {
	m := newTestManager()
	m.minPoolSize = 3

	w1 := putWorker(m, "a1", router.WorkerAwaitingReplacement)
	w1.rotationScheduledAt = time.Now()
	m.preconnectInflight[w1] = struct{}{}

	w2 := putWorker(m, "a2", router.WorkerAwaitingReplacement)
	w2.rotationScheduledAt = time.Now()
	m.preconnectInflight[w2] = struct{}{}

	wClosing := putWorker(m, "c1", router.WorkerClosing)
	wClosing.ActiveConns = 1
	w3 := putWorker(m, "a3", router.WorkerAwaitingReplacement)
	w3.rotationScheduledAt = time.Now()

	dispatched := 0
	m.preconnectDispatch = func(ctx context.Context, target *Worker) {
		dispatched++
	}

	m.dispatchPending(context.Background())

	if dispatched != 0 {
		t.Fatalf("footprint gate should block only when inflight/closing footprint is full, got %d", dispatched)
	}
}

func TestDispatchPendingForceClosesOverdueAwaiting(t *testing.T) {
	m := newTestManager()
	m.minPoolSize = 1
	m.rotationGrace = 10 * time.Millisecond

	busy := putWorker(m, "busy", router.WorkerAwaitingReplacement)
	busy.rotationScheduledAt = time.Now()
	m.preconnectInflight[busy] = struct{}{}

	overdue := putWorker(m, "overdue", router.WorkerAwaitingReplacement)
	overdue.rotationScheduledAt = time.Now().Add(-time.Second)

	m.dispatchPending(context.Background())

	if overdue.State != router.WorkerClosing {
		t.Fatalf("expected overdue awaiting worker to be force-closed, got %v", overdue.State)
	}

	select {
	case <-m.wakeup:
	default:
		t.Fatal("expected overdue grace close to trigger wakeup")
	}

	select {
	case <-m.grow:
	default:
		t.Fatal("expected overdue grace close to trigger grow")
	}
}

func TestDispatchOrphanExitsCleanly(t *testing.T) {
	m := newTestManager()
	w := putWorker(m, "gone", router.WorkerAwaitingReplacement)
	w.rotationScheduledAt = time.Now()
	m.preconnectInflight[w] = struct{}{}

	delete(m.workers, "gone")

	done := make(chan struct{})
	go func() {
		m.preconnectWorker(context.Background(), w)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("preconnectWorker should return quickly on orphan")
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, busy := m.preconnectInflight[w]; busy {
		t.Fatal("inflight entry should be cleaned up on return")
	}
}

func TestPreconnectInflightKeyedByPointer(t *testing.T) {
	m := newTestManager()

	wOld := &Worker{ID: "worker-7", Index: 7, State: router.WorkerAwaitingReplacement}
	wOld.rotationScheduledAt = time.Now()
	m.workers[wOld.ID] = wOld
	m.preconnectInflight[wOld] = struct{}{}

	delete(m.workers, wOld.ID)
	delete(m.preconnectInflight, wOld)

	wNew := &Worker{ID: "worker-7", Index: 7, State: router.WorkerAwaitingReplacement}
	wNew.rotationScheduledAt = time.Now()
	m.workers[wNew.ID] = wNew

	dispatched := 0
	m.preconnectDispatch = func(ctx context.Context, target *Worker) {
		if target == wNew {
			dispatched++
		}
	}

	m.dispatchPending(context.Background())

	if dispatched != 1 {
		t.Fatalf("new worker reusing same ID should still be dispatched, got %d", dispatched)
	}
}

func TestPreconnectGraceForceCloses(t *testing.T) {
	m := newTestManager()
	m.rotationGrace = 10 * time.Millisecond
	m.preconnectSem = make(chan struct{}, 1)

	w := putWorker(m, "stuck", router.WorkerAwaitingReplacement)
	w.rotationScheduledAt = time.Now().Add(-time.Second)

	m.preconnectWorker(context.Background(), w)

	if w.State != router.WorkerClosing {
		t.Fatalf("expected force-closed worker to enter Closing, got %v", w.State)
	}
}

func TestPreconnectPostCreateOwnershipLossStopsUnpublishedWorker(t *testing.T) {
	m := newTestManager()
	m.preconnectSem = make(chan struct{}, 1)

	target := putWorker(m, "awaiting", router.WorkerAwaitingReplacement)
	target.rotationScheduledAt = time.Now()

	staged := &Worker{
		ID:          "worker-new",
		Index:       42,
		State:       router.WorkerReady,
		processDone: make(chan struct{}),
	}
	m.usedIndexes[staged.Index] = true

	m.preconnectCreate = func(ctx context.Context, country string) (*Worker, error) {
		delete(m.workers, target.ID)
		return staged, nil
	}

	m.preconnectWorker(context.Background(), target)

	if _, ok := m.workers[staged.ID]; ok {
		t.Fatalf("staged worker %s must not be published after ownership loss", staged.ID)
	}
	if _, ok := m.usedIndexes[staged.Index]; ok {
		t.Fatalf("staged worker index %d should be released after ownership loss", staged.Index)
	}
	if staged.State != router.WorkerClosing {
		t.Fatalf("staged worker should be stopped after ownership loss, got %v", staged.State)
	}
}
