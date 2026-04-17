package session

import (
	"testing"
	"time"
)

func TestLookupNoSession(t *testing.T) {
	manager := NewManager(30 * time.Minute)
	if snapshot, ok := manager.Lookup("missing"); ok {
		t.Fatalf("expected missing session, got %+v", snapshot)
	}
}

func TestBindAndLookup(t *testing.T) {
	manager := NewManager(30 * time.Minute)

	snapshot, created := manager.Bind("sess1", "us", 0, "worker-0")
	if !created {
		t.Fatalf("expected created=true")
	}
	if snapshot.WorkerID != "worker-0" {
		t.Fatalf("expected workerID worker-0, got %q", snapshot.WorkerID)
	}
	if snapshot.TTL != 30*time.Minute {
		t.Fatalf("expected default TTL 30m, got %v", snapshot.TTL)
	}

	lookup, ok := manager.Lookup("sess1")
	if !ok {
		t.Fatalf("expected session to exist")
	}
	if lookup.WorkerID != "worker-0" {
		t.Fatalf("expected existing worker worker-0, got %q", lookup.WorkerID)
	}
}

func TestBindCustomTTL(t *testing.T) {
	manager := NewManager(30 * time.Minute)
	snapshot, _ := manager.Bind("sess1", "jp", 60*time.Minute, "worker-0")
	if snapshot.TTL != 60*time.Minute {
		t.Fatalf("expected custom TTL 60m, got %v", snapshot.TTL)
	}
}

func TestLookupExpiredSession(t *testing.T) {
	manager := NewManager(30 * time.Minute)
	manager.Bind("sess1", "us", 2*time.Millisecond, "worker-0")

	time.Sleep(10 * time.Millisecond)

	if _, ok := manager.Lookup("sess1"); ok {
		t.Fatalf("expected expired session to be absent")
	}
}

func TestRemoveByWorker(t *testing.T) {
	manager := NewManager(30 * time.Minute)

	manager.Bind("sess1", "us", 0, "worker-0")
	manager.Bind("sess2", "us", 0, "worker-0")
	manager.Bind("sess3", "jp", 0, "worker-1")

	manager.RemoveByWorker("worker-0")

	if _, ok := manager.Lookup("sess1"); ok {
		t.Fatalf("expected sess1 to be removed")
	}
	if _, ok := manager.Lookup("sess2"); ok {
		t.Fatalf("expected sess2 to be removed")
	}
	if snapshot, ok := manager.Lookup("sess3"); !ok || snapshot.WorkerID != "worker-1" {
		t.Fatalf("expected sess3 to remain on worker-1, got ok=%v snapshot=%+v", ok, snapshot)
	}
}

func TestCleanup(t *testing.T) {
	manager := NewManager(30 * time.Minute)
	manager.Bind("sess1", "us", 2*time.Millisecond, "worker-0")

	time.Sleep(10 * time.Millisecond)

	if removed := manager.Cleanup(); removed != 1 {
		t.Fatalf("expected 1 removed session, got %d", removed)
	}
}

func TestActiveSessionsForWorker(t *testing.T) {
	manager := NewManager(30 * time.Minute)
	manager.Bind("s1", "us", 0, "worker-0")
	manager.Bind("s2", "us", 0, "worker-0")

	if count := manager.ActiveSessionsForWorker("worker-0"); count != 2 {
		t.Fatalf("expected 2 active sessions, got %d", count)
	}
}
