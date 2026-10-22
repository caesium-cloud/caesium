package freshness

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// The start view (_consumed_watermarks_start) is read before the admission
// transaction freezes the run's namespace; FenceStartParams binds it to that
// namespace inside the transaction (identity-and-access B6). These drive the
// real run store with the production enricher and fence registered.

// registerStartViewHooks installs the production enricher (wrapped by between,
// which runs after the enricher's read and before admission) and the fence.
func registerStartViewHooks(t *testing.T, between func(db *gorm.DB, jobID uuid.UUID)) {
	t.Helper()
	runstorage.SetStartParamsEnricher(func(ctx context.Context, db *gorm.DB, jobID uuid.UUID, params map[string]string, fromQueue bool) (map[string]string, error) {
		out, err := EnrichStartParams(ctx, db, jobID, params, fromQueue)
		if between != nil {
			between(db, jobID)
		}
		return out, err
	})
	runstorage.SetStartParamsFence(FenceStartParams)
	t.Cleanup(func() {
		runstorage.SetStartParamsEnricher(nil)
		runstorage.SetStartParamsFence(nil)
	})
}

// seedFencedProducer writes a live job in namespace that produces
// staging.orders and consumes inputs.
func seedFencedProducer(t *testing.T, db *gorm.DB, namespace string, inputs ...string) uuid.UUID {
	t.Helper()
	jobID := uuid.New()
	if err := db.Create(&models.Job{ID: jobID, Alias: "fenced-" + jobID.String()[:8], Namespace: namespace}).Error; err != nil {
		t.Fatalf("create job: %v", err)
	}
	seedEnricherJob(t, db, jobID, []string{"staging.orders"}, inputs)
	return jobID
}

// completeFencedRun finishes a started run the way the executor does (its
// producing step succeeded, non-cached) and hands it to the capturer.
func completeFencedRun(t *testing.T, db *gorm.DB, c *Capturer, jobID, runID uuid.UUID) {
	t.Helper()
	now := t0.Add(time.Hour)
	taskID := uuid.New()
	if err := db.Create(&models.Task{ID: taskID, JobID: jobID, AtomID: uuid.New(), Name: "produce"}).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := db.Create(&models.TaskRun{
		ID: uuid.New(), JobRunID: runID, TaskID: taskID, AtomID: uuid.New(),
		Engine: "docker", Image: "etl:1.4", Command: "run", Status: taskRunTerminalSucceeded,
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("create task run: %v", err)
	}
	if err := db.Model(&models.JobRun{}).Where("id = ?", runID).
		Updates(map[string]any{"status": "succeeded", "completed_at": now}).Error; err != nil {
		t.Fatalf("complete run: %v", err)
	}
	c.handleRunCompleted(context.Background(), event.Event{Type: event.TypeRunCompleted, JobID: jobID, RunID: runID})
}

// runResponses returns the params the run detail and run list responses carry
// (run.Store.Get / List, which the REST run endpoints serialise).
func runResponses(t *testing.T, store *runstorage.Store, jobID, runID uuid.UUID) (detail, listed map[string]string, namespace string) {
	t.Helper()
	got, err := store.Get(runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	runs, _, _, err := store.List(jobID, 10, 0)
	if err != nil || len(runs) != 1 {
		t.Fatalf("list runs: %v (%d)", err, len(runs))
	}
	return got.Params, runs[0].Params, got.Namespace
}

// TestStartViewReadBeforeAMoveIsNotPersistedOnTheNewNamespacesRun is the Codex
// PR r1 P1: an apply moves the producer finance -> marketing and removes its
// input between the enricher's read and admission. The marketing run must not
// carry (or show) finance's input view, and completing it must not credit that
// input to the marketing run.
func TestStartViewReadBeforeAMoveIsNotPersistedOnTheNewNamespacesRun(t *testing.T) {
	db := openRegistryDB(t)
	ctx := context.Background()
	c := NewCapturer(event.New(), db)
	if _, err := c.store.Advance(ctx, AdvanceInput{Name: "raw.finance_ledger", Watermark: "finance-ledger-7", RunID: uuid.New(), CompletedAt: t0}); err != nil {
		t.Fatalf("seed finance input: %v", err)
	}
	jobID := seedFencedProducer(t, db, "finance", "raw.finance_ledger")

	moved := false
	registerStartViewHooks(t, func(db *gorm.DB, jobID uuid.UUID) {
		if moved {
			return
		}
		moved = true
		moveJob(t, db, jobID, "marketing")
		if err := db.Where("job_id = ? AND direction = ?", jobID, models.DatasetDirectionConsumes).Delete(&models.DatasetDeclaration{}).Error; err != nil {
			t.Fatalf("drop the input: %v", err)
		}
	})

	store := runstorage.NewStore(db)
	started, err := store.StartWithContext(ctx, jobID, nil, runstorage.WithStartParams(map[string]string{"logical_date": "2026-07-03"}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	detail, listed, namespace := runResponses(t, store, jobID, started.ID)
	if namespace != "marketing" {
		t.Fatalf("run namespace = %q, want marketing", namespace)
	}
	for name, params := range map[string]map[string]string{"detail": detail, "list": listed, "start result": started.Params} {
		if _, ok := params[ConsumedWatermarksStartParam]; ok {
			t.Fatalf("%s carries the view read through finance's declarations: %v", name, params)
		}
		if _, ok := params[startViewNamespaceParam]; ok {
			t.Fatalf("%s carries the transient capture namespace: %v", name, params)
		}
		if params["logical_date"] != "2026-07-03" {
			t.Fatalf("%s lost a caller param: %v", name, params)
		}
	}

	completeFencedRun(t, db, c, jobID, started.ID)
	st, ok, err := c.store.Get(ctx, nil, "staging.orders")
	if err != nil || !ok {
		t.Fatalf("produced state: ok=%v err=%v", ok, err)
	}
	if st.LastRunID == nil || *st.LastRunID != started.ID {
		t.Fatalf("the marketing run must still advance its output, last run = %v", st.LastRunID)
	}
	if len(st.ConsumedWatermarks) != 0 || st.ConsumedRunID != nil {
		t.Fatalf("finance's input was credited to the marketing run: %s (owner %v)", st.ConsumedWatermarks, st.ConsumedRunID)
	}
}

// TestStartViewIsRetakenForTheAdmittedNamespacesInputs: a move that changes
// the inputs re-takes the view, inside admission, from the inputs the
// admitted namespace's job declares.
func TestStartViewIsRetakenForTheAdmittedNamespacesInputs(t *testing.T) {
	db := openRegistryDB(t)
	ctx := context.Background()
	fstore := NewStore(db)
	for name, wm := range map[string]string{"raw.finance_ledger": "finance-ledger-7", "raw.campaigns": "campaigns-3"} {
		if _, err := fstore.Advance(ctx, AdvanceInput{Name: name, Watermark: wm, RunID: uuid.New(), CompletedAt: t0}); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	jobID := seedFencedProducer(t, db, "finance", "raw.finance_ledger")
	moved := false
	registerStartViewHooks(t, func(db *gorm.DB, jobID uuid.UUID) {
		if moved {
			return
		}
		moved = true
		moveJob(t, db, jobID, "marketing")
		if err := db.Where("job_id = ? AND direction = ?", jobID, models.DatasetDirectionConsumes).Delete(&models.DatasetDeclaration{}).Error; err != nil {
			t.Fatalf("drop the input: %v", err)
		}
		seedDeclarations(t, db, consumeDecl(jobID, "raw.campaigns"))
	})

	store := runstorage.NewStore(db)
	started, err := store.StartWithContext(ctx, jobID, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	detail, _, namespace := runResponses(t, store, jobID, started.ID)
	if namespace != "marketing" {
		t.Fatalf("run namespace = %q, want marketing", namespace)
	}
	if got := detail[ConsumedWatermarksStartParam]; got != `{"raw.campaigns":"campaigns-3"}` {
		t.Fatalf("start view = %q, want the admitted namespace's input only", got)
	}
	if _, ok := detail[startViewNamespaceParam]; ok {
		t.Fatalf("the transient capture namespace was persisted: %v", detail)
	}
}

// TestStartViewOfAnUnmovedRunIsPersistedAsStamped: without a move the fence
// only removes its transient marker, so the persisted params are exactly the
// enricher's (the no-policy and unmoved byte-identity rule).
func TestStartViewOfAnUnmovedRunIsPersistedAsStamped(t *testing.T) {
	db := openRegistryDB(t)
	ctx := context.Background()
	if _, err := NewStore(db).Advance(ctx, AdvanceInput{Name: "raw.vendor_x", Watermark: "vendor-key-1", RunID: uuid.New(), CompletedAt: t0}); err != nil {
		t.Fatalf("seed input: %v", err)
	}
	jobID := seedFencedProducer(t, db, "", "raw.vendor_x")
	registerStartViewHooks(t, nil)

	store := runstorage.NewStore(db)
	started, err := store.StartWithContext(ctx, jobID, nil, runstorage.WithStartParams(map[string]string{"logical_date": "2026-07-03"}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	var row models.JobRun
	if err := db.Select("params").First(&row, "id = ?", started.ID).Error; err != nil {
		t.Fatalf("load run: %v", err)
	}
	want, _ := json.Marshal(map[string]string{
		"logical_date":               "2026-07-03",
		ConsumedWatermarksStartParam: `{"raw.vendor_x":"vendor-key-1"}`,
	})
	if string(row.Params) != string(want) {
		t.Fatalf("persisted params = %s, want %s", row.Params, want)
	}
}

// TestFenceStartParamsRemovesTheViewWhenItCannotRetakeIt: a re-read that
// fails leaves no view behind (completion then takes its own fenced read),
// and keeps the caller's params.
func TestFenceStartParamsRemovesTheViewWhenItCannotRetakeIt(t *testing.T) {
	db := openRegistryDB(t)
	jobID := seedFencedProducer(t, db, "marketing", "raw.campaigns")
	if err := db.Migrator().DropTable(&models.DatasetState{}); err != nil {
		t.Fatalf("drop dataset_states: %v", err)
	}
	out, err := FenceStartParams(context.Background(), db, jobID, "marketing", map[string]string{
		"logical_date":               "2026-07-03",
		ConsumedWatermarksStartParam: `{"raw.finance_ledger":"finance-ledger-7"}`,
		startViewNamespaceParam:      "finance",
	})
	if err == nil {
		t.Fatal("a failed re-read must be reported")
	}
	if len(out) != 1 || out["logical_date"] != "2026-07-03" {
		t.Fatalf("fenced params = %v, want only the caller's", out)
	}

	// No transient marker (no view stamped, or no enricher): untouched.
	in := map[string]string{ConsumedWatermarksStartParam: `{"raw.x":"1"}`}
	if out, err := FenceStartParams(context.Background(), db, jobID, "finance", in); err != nil || len(out) != 1 || out[ConsumedWatermarksStartParam] != `{"raw.x":"1"}` {
		t.Fatalf("unmarked params were changed: %v (%v)", out, err)
	}
}
