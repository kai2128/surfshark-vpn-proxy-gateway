//go:build linux

package worker

import (
	"testing"
	"time"

	"surfshark-proxy/internal/router"
)

func newTestWorker(state router.WorkerState) *Worker {
	return &Worker{
		ID:    "worker-test",
		Index: 0,
		State: state,
	}
}

func TestMarkAwaitingReplacementSetsTimestamp(t *testing.T) {
	w := newTestWorker(router.WorkerReady)
	before := time.Now()

	if !w.markAwaitingReplacement() {
		t.Fatal("expected CAS to succeed from Ready")
	}
	if !w.isAwaitingReplacement() {
		t.Fatal("state should be AwaitingReplacement")
	}

	ts := w.rotationScheduledSince()
	if ts.Before(before) || ts.After(time.Now()) {
		t.Fatalf("rotationScheduledAt %v not in [%v, now]", ts, before)
	}
}

func TestMarkAwaitingReplacementFromNonEligibleStates(t *testing.T) {
	cases := []router.WorkerState{
		router.WorkerCreating,
		router.WorkerClosing,
		router.WorkerAwaitingReplacement,
	}

	for _, state := range cases {
		w := newTestWorker(state)
		if w.markAwaitingReplacement() {
			t.Fatalf("expected CAS to fail from state %v", state)
		}
	}
}

func TestMarkAwaitingReplacementFromIdle(t *testing.T) {
	w := newTestWorker(router.WorkerIdle)
	if !w.markAwaitingReplacement() {
		t.Fatal("expected CAS to succeed from Idle")
	}
}

func TestMarkClosingFromAwaiting(t *testing.T) {
	w := newTestWorker(router.WorkerAwaitingReplacement)
	if !w.markClosingFromAwaiting() {
		t.Fatal("expected CAS to succeed from AwaitingReplacement")
	}
	if w.markClosingFromAwaiting() {
		t.Fatal("expected second CAS to be no-op")
	}
}

func TestMarkClosingFromAwaitingRejectsOtherStates(t *testing.T) {
	cases := []router.WorkerState{
		router.WorkerReady,
		router.WorkerIdle,
		router.WorkerClosing,
		router.WorkerCreating,
	}

	for _, state := range cases {
		w := newTestWorker(state)
		if w.markClosingFromAwaiting() {
			t.Fatalf("expected CAS to fail from state %v", state)
		}
	}
}

func TestIncrDecrDoesNotEscapeAwaiting(t *testing.T) {
	w := newTestWorker(router.WorkerAwaitingReplacement)
	w.IncrConns()
	if w.State != router.WorkerAwaitingReplacement {
		t.Fatalf("IncrConns must not flip state out of Awaiting, got %v", w.State)
	}

	w.DecrConns()
	if w.State != router.WorkerAwaitingReplacement {
		t.Fatalf("DecrConns must not flip state out of Awaiting, got %v", w.State)
	}
}

func TestComputeEffectiveMaxLifetimeZeroJitter(t *testing.T) {
	got := computeEffectiveMaxLifetime(time.Hour, 0)
	if got != time.Hour {
		t.Fatalf("expected 1h when jitter=0, got %v", got)
	}
}

func TestComputeEffectiveMaxLifetimeJitterRange(t *testing.T) {
	for i := 0; i < 100; i++ {
		got := computeEffectiveMaxLifetime(time.Hour, 10)
		if got > time.Hour {
			t.Fatalf("jitter must be single-sided downward, got %v", got)
		}
		if got < time.Hour*9/10 {
			t.Fatalf("jitter exceeds 10%%, got %v", got)
		}
	}
}

func TestComputeEffectiveMaxLifetimeDisabled(t *testing.T) {
	got := computeEffectiveMaxLifetime(0, 10)
	if got != 0 {
		t.Fatalf("expected 0 when maxLifetime=0, got %v", got)
	}
}
