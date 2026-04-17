package parser

import (
	"testing"
	"time"
)

func TestParseBasicAuth(t *testing.T) {
	result := Parse("user", "pass")
	if result.Username != "user" {
		t.Fatalf("expected username 'user', got %q", result.Username)
	}
	if result.Country != "" {
		t.Fatalf("expected empty country, got %q", result.Country)
	}
	if result.SessionID != "" {
		t.Fatalf("expected empty sessionID, got %q", result.SessionID)
	}
	if result.SessionTTL != 0 {
		t.Fatalf("expected zero TTL, got %v", result.SessionTTL)
	}
}

func TestParseCountryOnly(t *testing.T) {
	result := Parse("user__cr.us", "pass")
	if result.Username != "user" {
		t.Fatalf("expected username 'user', got %q", result.Username)
	}
	if result.Country != "us" {
		t.Fatalf("expected country 'us', got %q", result.Country)
	}
}

func TestParseFullParams(t *testing.T) {
	result := Parse("user__cr.jp;sessid.abc123;sessttl.60", "pass")
	if result.Username != "user" {
		t.Fatalf("expected username 'user', got %q", result.Username)
	}
	if result.Country != "jp" {
		t.Fatalf("expected country 'jp', got %q", result.Country)
	}
	if result.SessionID != "abc123" {
		t.Fatalf("expected sessionID 'abc123', got %q", result.SessionID)
	}
	if result.SessionTTL != 60*time.Minute {
		t.Fatalf("expected TTL 60m, got %v", result.SessionTTL)
	}
}

func TestParseSessionWithoutTTL(t *testing.T) {
	result := Parse("user__sessid.mysession", "pass")
	if result.SessionID != "mysession" {
		t.Fatalf("expected sessionID 'mysession', got %q", result.SessionID)
	}
	if result.SessionTTL != 0 {
		t.Fatalf("expected zero TTL, got %v", result.SessionTTL)
	}
}

func TestParseInvalidTTL(t *testing.T) {
	result := Parse("user__sessttl.abc", "pass")
	if result.SessionTTL != 0 {
		t.Fatalf("expected zero TTL for invalid input, got %v", result.SessionTTL)
	}
}

func TestParseNoDoubleUnderscore(t *testing.T) {
	result := Parse("simpleuser", "pass")
	if result.Username != "simpleuser" {
		t.Fatalf("expected username 'simpleuser', got %q", result.Username)
	}
	if result.Country != "" || result.SessionID != "" {
		t.Fatalf("expected no params for simple username, got %+v", result)
	}
}
