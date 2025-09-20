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

func TestAtomsList(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[
			{"id":"1","engine":"docker","image":"alpine","command":"[\"echo\",\"hello\"]","created_at":"2024-01-01T00:00:00Z","updated_at":"2024-01-01T01:00:00Z"}
		]`))
	}))
	defer ts.Close()

	cfg := &config.Config{BaseURL: mustParse(t, ts.URL), HTTPTimeout: time.Second}
	client := New(cfg)

	atoms, err := client.Atoms().List(context.Background(), url.Values{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(atoms) != 1 {
		t.Fatalf("expected 1 atom, got %d", len(atoms))
	}

	if len(atoms[0].Command) != 2 || atoms[0].Command[0] != "echo" {
		t.Fatalf("unexpected command decoded: %#v", atoms[0].Command)
	}
}
