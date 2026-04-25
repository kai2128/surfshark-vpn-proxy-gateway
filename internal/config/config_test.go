package config

import (
	"os"
	"testing"
	"time"
)

func TestLoadPreconnectDefaults(t *testing.T) {
	os.Unsetenv("PRECONNECT_CONCURRENCY")
	os.Unsetenv("WORKER_LIFETIME_JITTER_PCT")
	os.Unsetenv("WORKER_ROTATION_GRACE_MINUTES")

	cfg := Load()
	if cfg.PreconnectConcurrency != 3 {
		t.Fatalf("expected default 3, got %d", cfg.PreconnectConcurrency)
	}
	if cfg.WorkerLifetimeJitterPct != 10 {
		t.Fatalf("expected default 10, got %d", cfg.WorkerLifetimeJitterPct)
	}
	if cfg.WorkerRotationGrace != 0 {
		t.Fatalf("expected default 0 (auto), got %v", cfg.WorkerRotationGrace)
	}
}

func TestLoadPreconnectClamps(t *testing.T) {
	os.Setenv("PRECONNECT_CONCURRENCY", "-5")
	os.Setenv("WORKER_LIFETIME_JITTER_PCT", "-1")
	os.Setenv("WORKER_ROTATION_GRACE_MINUTES", "7")
	defer func() {
		os.Unsetenv("PRECONNECT_CONCURRENCY")
		os.Unsetenv("WORKER_LIFETIME_JITTER_PCT")
		os.Unsetenv("WORKER_ROTATION_GRACE_MINUTES")
	}()

	cfg := Load()
	if cfg.PreconnectConcurrency != 1 {
		t.Fatalf("expected clamp to 1, got %d", cfg.PreconnectConcurrency)
	}
	if cfg.WorkerLifetimeJitterPct != 0 {
		t.Fatalf("expected clamp to 0, got %d", cfg.WorkerLifetimeJitterPct)
	}
	if cfg.WorkerRotationGrace != 7*time.Minute {
		t.Fatalf("expected 7 minutes, got %v", cfg.WorkerRotationGrace)
	}
}

func TestLoadJitterUpperClamp(t *testing.T) {
	os.Setenv("WORKER_LIFETIME_JITTER_PCT", "99")
	defer os.Unsetenv("WORKER_LIFETIME_JITTER_PCT")

	cfg := Load()
	if cfg.WorkerLifetimeJitterPct != 50 {
		t.Fatalf("expected clamp to 50, got %d", cfg.WorkerLifetimeJitterPct)
	}
}
