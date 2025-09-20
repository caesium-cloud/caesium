package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/cmd/console/config"
)

func TestTriggersList(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		if r.URL.Query().Get("type") != "http" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[
			{"id":"1","alias":"trigger-a","type":"http","configuration":"{}","created_at":"2024-01-01T00:00:00Z","updated_at":"2024-01-01T01:00:00Z"}
		]`))
	}))
	defer ts.Close()

	cfg := &config.Config{BaseURL: mustParse(t, ts.URL), HTTPTimeout: time.Second}
	client := New(cfg)

	params := url.Values{}
	params.Set("type", "http")

	triggers, err := client.Triggers().List(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(triggers) != 1 {
		t.Fatalf("expected 1 trigger, got %d", len(triggers))
	}

	if triggers[0].Alias != "trigger-a" {
		t.Fatalf("unexpected alias %q", triggers[0].Alias)
	}
}
