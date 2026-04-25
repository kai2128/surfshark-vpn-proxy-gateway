package router

import (
	"testing"
	"time"

	"surfshark-proxy/internal/parser"
	"surfshark-proxy/internal/session"
)

type mockWorkerPool struct {
	workers map[string]*WorkerInfo
}

func (m *mockWorkerPool) GetReadyWorkers(country string) []*WorkerInfo {
	var result []*WorkerInfo
	for _, worker := range m.workers {
		switch worker.State {
		case WorkerReady, WorkerIdle, WorkerAwaitingReplacement:
			if country == "" || worker.Country == country {
				result = append(result, worker)
			}
		}
	}
	return result
}

func (m *mockWorkerPool) GetRoutableWorkers(country string) []*WorkerInfo {
	var active []*WorkerInfo
	var awaiting []*WorkerInfo

	for _, worker := range m.workers {
		if country != "" && worker.Country != country {
			continue
		}

		switch worker.State {
		case WorkerReady, WorkerIdle:
			active = append(active, worker)
		case WorkerAwaitingReplacement:
			awaiting = append(awaiting, worker)
		}
	}

	if len(active) > 0 {
		return active
	}
	return awaiting
}

func (m *mockWorkerPool) RequestWorker(country string) (*WorkerInfo, error) {
	id := "new-worker-" + country
	worker := &WorkerInfo{ID: id, Country: country, State: WorkerReady}
	m.workers[id] = worker
	return worker, nil
}

func TestRotatingNoCountry(t *testing.T) {
	pool := &mockWorkerPool{
		workers: map[string]*WorkerInfo{
			"w0": {ID: "w0", Country: "us", State: WorkerReady},
			"w1": {ID: "w1", Country: "jp", State: WorkerReady},
		},
	}
	router := New(pool, session.NewManager(30*time.Minute))

	first, err := router.Route(parser.Params{Username: "user"})
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}
	second, err := router.Route(parser.Params{Username: "user"})
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}

	if first.ID == second.ID {
		t.Fatalf("expected round robin to rotate workers, got %s twice", first.ID)
	}
}

func TestRotatingWithCountry(t *testing.T) {
	pool := &mockWorkerPool{
		workers: map[string]*WorkerInfo{
			"w0": {ID: "w0", Country: "us", State: WorkerReady},
			"w1": {ID: "w1", Country: "jp", State: WorkerReady},
		},
	}
	router := New(pool, session.NewManager(30*time.Minute))

	worker, err := router.Route(parser.Params{Username: "user", Country: "jp"})
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}
	if worker.Country != "jp" {
		t.Fatalf("expected country jp, got %q", worker.Country)
	}
}

func TestStickySession(t *testing.T) {
	pool := &mockWorkerPool{
		workers: map[string]*WorkerInfo{
			"w0": {ID: "w0", Country: "us", State: WorkerReady},
			"w1": {ID: "w1", Country: "us", State: WorkerReady},
		},
	}
	sessionManager := session.NewManager(30 * time.Minute)
	router := New(pool, sessionManager)

	params := parser.Params{Username: "user", Country: "us", SessionID: "sess1"}
	first, err := router.Route(params)
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}
	second, err := router.Route(params)
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}

	if first.ID != second.ID {
		t.Fatalf("expected sticky session to stay on same worker, got %s and %s", first.ID, second.ID)
	}

	snapshot, ok := sessionManager.Lookup("sess1")
	if !ok {
		t.Fatalf("expected session to exist")
	}
	if snapshot.WorkerID != first.ID {
		t.Fatalf("expected session bound to %s, got %s", first.ID, snapshot.WorkerID)
	}
}

func TestRequestWorkerOnDemand(t *testing.T) {
	pool := &mockWorkerPool{workers: map[string]*WorkerInfo{}}
	router := New(pool, session.NewManager(30*time.Minute))

	worker, err := router.Route(parser.Params{Username: "user", Country: "de"})
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}
	if worker.Country != "de" {
		t.Fatalf("expected country de, got %q", worker.Country)
	}
}

func TestWorkerAwaitingReplacementConstantExists(t *testing.T) {
	states := map[WorkerState]string{
		WorkerCreating:            "Creating",
		WorkerReady:               "Ready",
		WorkerIdle:                "Idle",
		WorkerClosing:             "Closing",
		WorkerAwaitingReplacement: "AwaitingReplacement",
	}
	if len(states) != 5 {
		t.Fatalf("expected 5 distinct worker states, got %d", len(states))
	}
}

func TestGetRoutableWorkersExcludesAwaitingReplacement(t *testing.T) {
	pool := &mockWorkerPool{
		workers: map[string]*WorkerInfo{
			"w0": {ID: "w0", Country: "us", State: WorkerReady},
			"w1": {ID: "w1", Country: "us", State: WorkerAwaitingReplacement},
		},
	}

	routable := pool.GetRoutableWorkers("us")
	if len(routable) != 1 || routable[0].ID != "w0" {
		t.Fatalf("expected only w0 in routable set, got %+v", routable)
	}
}

func TestGetRoutableWorkersFallsBackWhenOnlyAwaiting(t *testing.T) {
	pool := &mockWorkerPool{
		workers: map[string]*WorkerInfo{
			"w1": {ID: "w1", Country: "us", State: WorkerAwaitingReplacement},
		},
	}

	routable := pool.GetRoutableWorkers("us")
	if len(routable) != 1 || routable[0].ID != "w1" {
		t.Fatalf("expected fallback to awaiting worker, got %+v", routable)
	}
}

func TestSelectOrCreatePrefersReadyOverAwaiting(t *testing.T) {
	pool := &mockWorkerPool{
		workers: map[string]*WorkerInfo{
			"w0": {ID: "w0", Country: "us", State: WorkerAwaitingReplacement},
			"w1": {ID: "w1", Country: "us", State: WorkerReady},
		},
	}
	router := New(pool, session.NewManager(30*time.Minute))

	worker, err := router.Route(parser.Params{Username: "user", Country: "us"})
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}
	if worker.ID != "w1" {
		t.Fatalf("expected routing to prefer Ready w1, got %s", worker.ID)
	}
}

func TestStickyStaysOnAwaitingReplacement(t *testing.T) {
	pool := &mockWorkerPool{
		workers: map[string]*WorkerInfo{
			"w0": {ID: "w0", Country: "us", State: WorkerReady},
			"w1": {ID: "w1", Country: "us", State: WorkerReady},
		},
	}
	sessionManager := session.NewManager(30 * time.Minute)
	router := New(pool, sessionManager)

	params := parser.Params{Username: "user", Country: "us", SessionID: "sess-sticky"}
	first, err := router.Route(params)
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}

	pool.workers[first.ID].State = WorkerAwaitingReplacement

	second, err := router.Route(params)
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("expected sticky to stay on %s through AwaitingReplacement, got %s", first.ID, second.ID)
	}
}
