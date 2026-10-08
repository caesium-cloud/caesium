package run

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestColdInjectedServiceDoesNotOpenDefaultDatabase(t *testing.T) {
	const childEnv = "CAESIUM_TEST_COLD_RUN_SERVICE"
	if os.Getenv(childEnv) != "1" {
		executable, err := os.Executable()
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		child := exec.CommandContext(ctx, executable, "-test.run=^TestColdInjectedServiceDoesNotOpenDefaultDatabase$")
		child.Env = append(os.Environ(), childEnv+"=1")
		var stdout, stderr bytes.Buffer
		child.Stdout, child.Stderr = &stdout, &stderr
		err = child.Run()
		require.NoError(t, err, "cold injected service must not initialize the unrelated router; stdout=%s stderr=%s", stdout.String(), stderr.String())
		require.Contains(t, stdout.String(), "cold-injected-service-passed")
		return
	}

	// A default-router lookup would fail while parsing this DSN, before any
	// network connection. The fresh subprocess cannot inherit an initialized
	// default store from another test and hide an eager-construction regression.
	t.Setenv("CAESIUM_DATABASE_TYPE", "postgres")
	t.Setenv("CAESIUM_DATABASE_DSN", "port=not-a-number")
	t.Setenv("CAESIUM_DATABASE_SHARDS", "1")
	require.NoError(t, env.Process())

	for _, binding := range []string{"store", "database"} {
		t.Run(binding, func(t *testing.T) {
			conn := jobdeftestutil.OpenTestDB(t)
			t.Cleanup(func() { jobdeftestutil.CloseDB(conn) })
			j := models.Job{ID: uuid.New(), Alias: "cold-service-" + binding}
			require.NoError(t, conn.Create(&j).Error)
			type requestKey struct{}
			requestCtx, cancel := context.WithTimeout(context.WithValue(t.Context(), requestKey{}, binding), time.Minute)
			cancel()
			svc := New(requestCtx).(*runService)
			require.Nil(t, svc.store, "construction alone must not resolve the process default")
			require.Same(t, svc, svc.WithStore(nil))
			require.Same(t, svc, svc.WithDatabase(nil))
			require.Nil(t, svc.store, "nil bindings remain no-ops")
			if binding == "store" {
				store := runstorage.NewStore(conn)
				require.Same(t, svc, svc.WithStore(store))
				require.Same(t, store, svc.runStore())
			} else {
				require.Same(t, svc, svc.WithDatabase(conn))
			}
			observed := false
			require.NoError(t, conn.Callback().Create().Before("gorm:create").Register("test:service_context", func(tx *gorm.DB) {
				if tx.Statement.Schema == nil || tx.Statement.Schema.Name != "JobRun" {
					return
				}
				observed = true
				require.Equal(t, binding, tx.Statement.Context.Value(requestKey{}))
				require.NoError(t, tx.Statement.Context.Err())
				_, deadline := tx.Statement.Context.Deadline()
				require.False(t, deadline)
			}))
			opts := []runstorage.StartOption{
				nil, // optional nil entries retain the Store option policy
				runstorage.WithStartParams(map[string]string{"binding": binding}),
				runstorage.WithStartIdempotencyKey("cold-service-" + binding),
			}
			result, err := svc.StartWithResult(j.ID, nil, opts...)
			require.NoError(t, err)
			require.True(t, observed)
			require.Equal(t, runstorage.StartOutcomeCreated, result.Outcome)
			require.NotNil(t, result.Run)
			require.Equal(t, binding, result.Run.Params["binding"])
			replayed, found, err := svc.FindIdempotentStart(j.ID, opts...)
			require.NoError(t, err)
			require.True(t, found)
			require.True(t, replayed.Replayed)
			require.Equal(t, result.Run.ID, replayed.Run.ID)
			var count int64
			require.NoError(t, conn.Model(&models.JobRun{}).Count(&count).Error)
			require.EqualValues(t, 1, count)
			_, err = svc.Get(result.Run.ID)
			require.NoError(t, err)
			nilContext := New(nil).WithStore(svc.runStore()) //nolint:staticcheck // API intentionally accepts nil; detachedContext falls back to Background.
			_, found, err = nilContext.FindIdempotentStart(j.ID, opts...)
			require.NoError(t, err)
			require.True(t, found)
		})
	}
	fmt.Fprintln(os.Stdout, "cold-injected-service-passed")
}

func isolateDefaultService(t *testing.T) {
	t.Helper()
	defaultServiceMu.Lock()
	previous := defaultService
	defaultService = nil
	defaultServiceMu.Unlock()
	t.Cleanup(func() {
		defaultServiceMu.Lock()
		defaultService = previous
		defaultServiceMu.Unlock()
	})
}

func TestSetBusPublishesCapturedStoreWithoutChangingFirstDefault(t *testing.T) {
	isolateDefaultService(t)
	firstDB, secondDB := jobdeftestutil.OpenTestDB(t), jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(firstDB); jobdeftestutil.CloseDB(secondDB) })
	first, second := runstorage.NewStore(firstDB), runstorage.NewStore(secondDB)
	firstBus, secondBus := event.New(), event.New()
	firstService := New(t.Context()).WithStore(first)
	firstService.SetBus(firstBus)
	inherited := New(t.Context()).(*runService)
	require.Same(t, first, inherited.runStore())
	require.Same(t, firstBus, first.Bus())
	secondService := New(t.Context()).WithStore(second)
	secondService.SetBus(secondBus)
	require.Same(t, secondBus, second.Bus())
	require.Same(t, firstBus, first.Bus())
	require.Same(t, first, New(t.Context()).(*runService).runStore(), "later SetBus must not replace the first default")
	firstService.WithStore(second)
	require.Same(t, first, inherited.runStore(), "New captures an initialized singleton's binding")
	require.Same(t, first, New(t.Context()).(*runService).runStore(), "rebinding one wrapper must not change the singleton")
}

func TestConcurrentServiceReadsCaptureAnExplicitBinding(t *testing.T) {
	isolateDefaultService(t)
	firstDB, secondDB := jobdeftestutil.OpenTestDB(t), jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(firstDB); jobdeftestutil.CloseDB(secondDB) })
	jobID, runID := uuid.New(), uuid.New()
	for name, conn := range map[string]*gorm.DB{"first": firstDB, "second": secondDB} {
		require.NoError(t, conn.Create(&models.Job{ID: jobID, Alias: name}).Error)
		require.NoError(t, conn.Create(&models.JobRun{ID: runID, JobID: jobID, Status: string(runstorage.StatusRunning)}).Error)
	}
	first, second := runstorage.NewStore(firstDB), runstorage.NewStore(secondDB)
	svc := New(t.Context()).WithStore(first)
	var workers sync.WaitGroup
	start := make(chan struct{})
	errors := make(chan error, 8)
	for worker := 0; worker < 8; worker++ {
		workers.Go(func() {
			<-start
			for iteration := 0; iteration < 20; iteration++ {
				row, err := svc.Get(runID)
				if err != nil {
					errors <- err
					return
				}
				if row.ID != runID || (row.JobAlias != "first" && row.JobAlias != "second") {
					errors <- fmt.Errorf("read escaped the explicit bindings: %+v", row)
					return
				}
			}
		})
	}
	workers.Go(func() {
		<-start
		for iteration := 0; iteration < 20; iteration++ {
			svc.WithStore(second)
			svc.WithDatabase(firstDB)
			svc.WithStore(first)
		}
	})
	close(start)
	workers.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	require.Nil(t, defaultService, "explicit operations must not publish a process default")
}
