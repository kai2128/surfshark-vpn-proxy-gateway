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
		if worker.State != WorkerReady {
			continue
		}
		if country == "" || worker.Country == country {
			result = append(result, worker)
		}
	}
	return result
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
