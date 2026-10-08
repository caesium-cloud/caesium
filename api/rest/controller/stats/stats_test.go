package stats

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	statsservice "github.com/caesium-cloud/caesium/api/rest/service/stats"
	"github.com/caesium-cloud/caesium/internal/testutil"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/labstack/echo/v5"
)

const statsNativeChild = "CAESIUM_STATS_NATIVE_CHILD"

// The native app is process-global and has no exported shutdown/reset. A fresh
// child also prevents repeated -count runs from reusing a closed DefaultRouter.
func TestStatsNativeControllerCancellation(t *testing.T) {
	if os.Getenv(statsNativeChild) == "1" {
		statsNativeControllerChecks(t)
		return
	}
	dataDir, address := t.TempDir(), testutil.FreeLoopbackAddress(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal("locate stats test executable")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary,
		"-test.run=^TestStatsNativeControllerCancellation$", "-test.timeout=120s")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "CAESIUM_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, statsNativeChild+"=1",
		"CAESIUM_DATABASE_TYPE=internal", "CAESIUM_DATABASE_PATH="+dataDir,
		"CAESIUM_DATABASE_SHARDS=1", "CAESIUM_NODE_ADDRESS="+address,
		"CAESIUM_DATABASE_NODES=", "CAESIUM_DATABASE_BOOTSTRAP_PEERS=",
		"CAESIUM_DATABASE_VOTERS=3", "CAESIUM_DATABASE_STANDBYS=0",
		"CAESIUM_AUTH_MODE=none", "CAESIUM_CONNECTORS_ENABLED=false",
		"CAESIUM_AGENT_REMEDIATION_ENABLED=false", "CAESIUM_LOG_LEVEL=error")
	// No child SQL/config logs are copied into parent artifacts. Run waits even
	// after cancellation, before t.TempDir cleanup can remove the native data.
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			t.Fatal("native stats child exceeded its bounded deadline and was joined")
		}
		t.Fatalf("native stats child failed and was joined (%T)", err)
	}
}

func statsNativeControllerChecks(t *testing.T) {
	t.Helper()
	if err := env.Process(); err != nil {
		t.Fatalf("process isolated native environment (%T)", err)
	}
	config := env.Variables()
	if config.DatabaseType != "internal" || config.DatabasePath == "" ||
		config.DatabasePath != os.Getenv("CAESIUM_DATABASE_PATH") ||
		config.NodeAddress == "" || config.NodeAddress != os.Getenv("CAESIUM_NODE_ADDRESS") ||
		config.DatabaseShards != 1 || len(config.DatabaseNodes) != 0 ||
		len(config.DatabaseBootstrapPeers) != 0 || config.DatabaseVoters != 3 ||
		config.DatabaseStandbys != 0 || config.AuthMode != "none" ||
		config.ConnectorsEnabled || config.AgentRemediationEnabled {
		t.Fatal("native stats environment was not isolated")
	}
	router := db.DefaultRouter()
	t.Cleanup(func() {
		if err := router.Close(); err != nil {
			t.Errorf("close owned native SQL pools (%T)", err)
		}
	})
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate owned native database (%T)", err)
	}
	e := echo.New()
	var observed error
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			observed = next(c)
			return observed // Preserve normal HTTP error handling and wrapping.
		}
	})
	e.GET("/v1/stats", Get)
	e.GET("/v1/stats/summary", Summary)
	for _, route := range []struct {
		path string
		bins int
	}{
		{"/v1/stats", 7},
		{"/v1/stats/summary", 7},
		{"/v1/stats/summary?window=24h", 24},
		{"/v1/stats/summary?window=30d", 30},
	} {
		t.Run(route.path, func(t *testing.T) {
			request := func(ctx context.Context) *httptest.ResponseRecorder {
				observed = nil
				recorder := httptest.NewRecorder()
				req := httptest.NewRequestWithContext(ctx, http.MethodGet, route.path, nil)
				e.ServeHTTP(recorder, req)
				return recorder
			}
			positive := func() {
				t.Helper()
				recorder := request(t.Context())
				if observed != nil || recorder.Code != http.StatusOK {
					t.Fatalf("native positive stats request failed (status %d)", recorder.Code)
				}
				var result statsservice.StatsResponse
				if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
					t.Fatal("positive stats response was not JSON")
				}
				if result.Jobs != (statsservice.JobStats{}) || len(result.TopFailing) != 0 ||
					len(result.TopFailingAtoms) != 0 || len(result.SlowestJobs) != 0 ||
					len(result.SuccessRateTrend) != route.bins {
					t.Fatal("positive stats response did not describe the empty native database")
				}
				for _, bin := range result.SuccessRateTrend {
					if bin.Date == "" || bin.RunCount != 0 || bin.SuccessRate != 0 {
						t.Fatal("positive stats trend contained invalid empty-database values")
					}
				}
			}
			positive()
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			recorder := request(ctx)
			httpErr, ok := errors.AsType[*echo.HTTPError](observed)
			if !errors.Is(observed, context.Canceled) || !ok ||
				httpErr.Code != http.StatusInternalServerError || recorder.Code != http.StatusInternalServerError {
				t.Fatal("native canceled query did not preserve its cause and return HTTP 500")
			}
			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil ||
				len(body) != 1 || body["message"] != "internal server error" {
				t.Fatal("native canceled-query response was not exact redacted JSON")
			}
			positive()
		})
	}
}
