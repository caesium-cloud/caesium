package lineage

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestBuildTransportHTTP(t *testing.T) {
	transport, err := BuildTransport(Config{
		Transport: "http",
		URL:       "http://localhost:5000/api/v1/lineage",
		Timeout:   5 * time.Second,
	})
	if err != nil {
		t.Fatalf("BuildTransport: %v", err)
	}
	if transport == nil {
		t.Fatal("transport is nil")
	}
}

func TestBuildTransportHTTPMissingURL(t *testing.T) {
	_, err := BuildTransport(Config{
		Transport: "http",
		URL:       "",
	})
	if err == nil {
		t.Fatal("expected error for missing URL")
	}
	if !strings.Contains(err.Error(), "CAESIUM_OPEN_LINEAGE_URL") {
		t.Errorf("error = %v, want mention of CAESIUM_OPEN_LINEAGE_URL", err)
	}
}

func TestBuildTransportConsole(t *testing.T) {
	transport, err := BuildTransport(Config{
		Transport: "console",
	})
	if err != nil {
		t.Fatalf("BuildTransport: %v", err)
	}
	if transport == nil {
		t.Fatal("transport is nil")
	}
}

func TestBuildTransportFile(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "lineage-config-test-*.ndjson")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	tmpPath := tmpFile.Name()
	if err := tmpFile.Close(); err != nil {
		t.Fatalf("close temp file: %v", err)
	}
	defer func() {
		if err := os.Remove(tmpPath); err != nil {
			t.Fatalf("remove temp file: %v", err)
		}
	}()

	transport, err := BuildTransport(Config{
		Transport: "file",
		FilePath:  tmpPath,
	})
	if err != nil {
		t.Fatalf("BuildTransport: %v", err)
	}
	if transport == nil {
		t.Fatal("transport is nil")
	}
	if err := transport.Close(); err != nil {
		t.Fatalf("close transport: %v", err)
	}
}

func TestBuildTransportUnknown(t *testing.T) {
	_, err := BuildTransport(Config{
		Transport: "kafka",
	})
	if err == nil {
		t.Fatal("expected error for unsupported transport")
	}
	if !strings.Contains(err.Error(), "kafka") {
		t.Errorf("error = %v, want mention of 'kafka'", err)
	}
}

func TestParseHeaders(t *testing.T) {
	tests := []struct {
		input    string
		expected map[string]string
	}{
		{"", map[string]string{}},
		{"Authorization=Bearer token123", map[string]string{"Authorization": "Bearer token123"}},
		{"X-Api-Key=abc, X-Custom=def", map[string]string{"X-Api-Key": "abc", "X-Custom": "def"}},
		{" key = value ", map[string]string{"key": "value"}},
	}

	for _, tt := range tests {
		result := parseHeaders(tt.input)
		for k, v := range tt.expected {
			if result[k] != v {
				t.Errorf("parseHeaders(%q)[%q] = %q, want %q", tt.input, k, result[k], v)
			}
		}
		if len(result) != len(tt.expected) {
			t.Errorf("parseHeaders(%q) returned %d entries, want %d", tt.input, len(result), len(tt.expected))
		}
	}
}
