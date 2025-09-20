package config

import "testing"

func TestLoadDefaults(t *testing.T) {
	t.Setenv("CAESIUM_BASE_URL", "")
	t.Setenv("CAESIUM_HOST", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	expected := "http://127.0.0.1:8080"
	if got := cfg.BaseURL.String(); got != expected {
		t.Fatalf("expected base URL %q, got %q", expected, got)
	}
}

func TestLoadHostFallback(t *testing.T) {
	t.Setenv("CAESIUM_BASE_URL", "")
	t.Setenv("CAESIUM_HOST", "caesium.example.com")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	expected := "http://caesium.example.com:8080"
	if got := cfg.BaseURL.String(); got != expected {
		t.Fatalf("expected base URL %q, got %q", expected, got)
	}
}

func TestLoadBaseURL(t *testing.T) {
	base := "https://api.example.com:9999"
	t.Setenv("CAESIUM_BASE_URL", base)
	t.Setenv("CAESIUM_HOST", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if got := cfg.BaseURL.String(); got != base {
		t.Fatalf("expected base URL %q, got %q", base, got)
	}
}

func TestLoadInvalidURL(t *testing.T) {
	t.Setenv("CAESIUM_BASE_URL", "://bad")
	t.Setenv("CAESIUM_HOST", "")

	if _, err := Load(); err == nil {
		t.Fatal("expected error for invalid base URL")
	}
}

func TestLoadHostWithSchemeAndPort(t *testing.T) {
	t.Setenv("CAESIUM_BASE_URL", "")
	t.Setenv("CAESIUM_HOST", "https://demo.example.com:9090")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	expected := "https://demo.example.com:9090"
	if got := cfg.BaseURL.String(); got != expected {
		t.Fatalf("expected base URL %q, got %q", expected, got)
	}
}
