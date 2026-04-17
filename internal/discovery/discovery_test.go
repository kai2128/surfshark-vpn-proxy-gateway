package discovery

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseServerName(t *testing.T) {
	tests := []struct {
		filename string
		country  string
		server   string
	}{
		{"us-dal.prod.surfshark.com_udp.ovpn", "us", "us-dal"},
		{"jp-tok.prod.surfshark.com_udp.ovpn", "jp", "jp-tok"},
		{"de-fra.prod.surfshark.com_tcp.ovpn", "de", "de-fra"},
		{"uk-lon.prod.surfshark.com_udp.ovpn", "uk", "uk-lon"},
		{"us-nyc.prod.surfshark.com_udp.ovpn", "us", "us-nyc"},
	}

	for _, testCase := range tests {
		country, server := parseFilename(testCase.filename)
		if country != testCase.country {
			t.Fatalf("parseFilename(%q) country = %q, want %q", testCase.filename, country, testCase.country)
		}
		if server != testCase.server {
			t.Fatalf("parseFilename(%q) server = %q, want %q", testCase.filename, server, testCase.server)
		}
	}
}

func TestParseInvalidFilename(t *testing.T) {
	country, server := parseFilename("not-a-valid-file.txt")
	if country != "" || server != "" {
		t.Fatalf("expected empty result, got country=%q server=%q", country, server)
	}
}

func TestScanDirectory(t *testing.T) {
	dir := t.TempDir()
	files := []string{
		"us-dal.prod.surfshark.com_udp.ovpn",
		"us-nyc.prod.surfshark.com_udp.ovpn",
		"jp-tok.prod.surfshark.com_udp.ovpn",
		"de-fra.prod.surfshark.com_tcp.ovpn",
		"readme.txt",
	}

	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("client\n"), 0o644); err != nil {
			t.Fatalf("write temp file %s: %v", name, err)
		}
	}

	servers, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan failed: %v", err)
	}

	if len(servers["us"]) != 2 {
		t.Fatalf("expected 2 US servers, got %d", len(servers["us"]))
	}
	if len(servers["jp"]) != 1 {
		t.Fatalf("expected 1 JP server, got %d", len(servers["jp"]))
	}
	if len(servers["de"]) != 1 {
		t.Fatalf("expected 1 DE server, got %d", len(servers["de"]))
	}

	total := 0
	for _, group := range servers {
		total += len(group)
	}
	if total != 4 {
		t.Fatalf("expected 4 total servers, got %d", total)
	}
}

func TestScanEmptyDirectory(t *testing.T) {
	servers, err := Scan(t.TempDir())
	if err != nil {
		t.Fatalf("Scan failed: %v", err)
	}
	if len(servers) != 0 {
		t.Fatalf("expected 0 countries, got %d", len(servers))
	}
}
