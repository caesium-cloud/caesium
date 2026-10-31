package lineage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	illineage "github.com/caesium-cloud/caesium/internal/lineage"
	"github.com/caesium-cloud/caesium/internal/models"
	dbfaults "github.com/caesium-cloud/caesium/internal/testutil"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type lineageWorld struct {
	t    *testing.T
	db   *gorm.DB
	tick time.Time
}

func newLineageWorld(t *testing.T) *lineageWorld {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	return &lineageWorld{t: t, db: db, tick: time.Now().UTC().Add(-time.Hour)}
}

func (w *lineageWorld) job(alias, namespace string) models.Job {
	triggerID := uuid.New()
	require.NoError(w.t, w.db.Create(&models.Trigger{ID: triggerID, Type: models.TriggerTypeCron}).Error)
	job := models.Job{ID: uuid.New(), Alias: alias, Namespace: namespace, TriggerID: triggerID, ProvenanceCommit: "commit-" + alias, ProvenanceRepo: "repo-" + alias}
	require.NoError(w.t, w.db.Create(&job).Error)
	return job
}

// run records one run of job, persisted in runNamespace, that read inputs and
// wrote outputs (OpenLineage namespace "ol").
func (w *lineageWorld) run(job models.Job, runNamespace string, inputs, outputs []string) uuid.UUID {
	atomID := uuid.New()
	require.NoError(w.t, w.db.Create(&models.Atom{ID: atomID, Engine: models.AtomEngineDocker, Image: "busybox:1.36.1"}).Error)
	task := models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atomID, Name: "step-" + job.Alias}
	require.NoError(w.t, w.db.Create(&task).Error)
	jobRun := models.JobRun{ID: uuid.New(), JobID: job.ID, TriggerID: job.TriggerID, Namespace: runNamespace, Status: "succeeded", StartedAt: w.tick}
	require.NoError(w.t, w.db.Create(&jobRun).Error)
	taskRun := models.TaskRun{ID: uuid.New(), JobRunID: jobRun.ID, TaskID: task.ID, AtomID: atomID, Engine: models.AtomEngineDocker, Image: "busybox:1.36.1", Command: "[]", Status: "succeeded"}
	require.NoError(w.t, w.db.Create(&taskRun).Error)
	add := func(name, direction string) {
		w.tick = w.tick.Add(time.Second)
		require.NoError(w.t, w.db.Create(&models.LineageDataset{
			ID: uuid.New(), TaskRunID: taskRun.ID, Namespace: "ol", Name: name, Direction: direction,
			FacetSummary: []byte(`{"caesium_dataset":{"step_name":"step-` + job.Alias + `"}}`), CreatedAt: w.tick,
		}).Error)
	}
	for _, name := range inputs {
		add(name, "input")
	}
	for _, name := range outputs {
		add(name, "output")
	}
	return taskRun.ID
}

func confinedTo(namespaces ...string) context.Context {
	return authz.WithAllowedNamespaces(context.Background(), namespaces)
}

func (w *lineageWorld) confined(ctx context.Context, namespace, root string) (*RedactedImpactResult, []byte) {
	r := authz.NewRedactor(ctx)
	require.NotNil(w.t, r)
	got, err := NewWithDatabase(ctx, w.db).ConfinedImpact(r, namespace, root, 0)
	require.NoError(w.t, err)
	body, err := json.Marshal(got)
	require.NoError(w.t, err)
	return got, body
}

// The mkt -> fin -> mkt chain: the finance hop is a placeholder that keeps its
// depth, the marketing node past it is shown whole, and nothing of finance
// reaches the body.
func TestConfinedImpactCollapsesForeignNodes(t *testing.T) {
	w := newLineageWorld(t)
	m1 := w.job("b5-m1", "marketing")
	f1 := w.job("b5-f1-secret", "finance")
	m2 := w.job("b5-m2", "marketing")
	w.run(m1, "marketing", nil, []string{"r-root"})
	w.run(f1, "finance", []string{"r-root"}, []string{"fin-dataset-secret"})
	w.run(m2, "marketing", []string{"fin-dataset-secret"}, []string{"m-out"})

	got, body := w.confined(confinedTo("marketing"), "ol", "r-root")
	require.Len(t, got.Downstream, 2)
	placeholder, ok := got.Downstream[0].(ExternalImpactNode)
	require.True(t, ok, "%T", got.Downstream[0])
	require.Equal(t, "external", placeholder.Kind)
	require.Equal(t, 0, placeholder.Depth)
	require.True(t, strings.HasPrefix(placeholder.ID, "external:"))
	visible, ok := got.Downstream[1].(illineage.ImpactNode)
	require.True(t, ok, "%T", got.Downstream[1])
	require.Equal(t, "m-out", visible.DatasetName)
	require.Equal(t, 1, visible.Depth)
	require.Equal(t, m2.ID, visible.JobID)

	for _, secret := range []string{"b5-f1-secret", "fin-dataset-secret", f1.ID.String(), "commit-b5-f1-secret", "repo-b5-f1-secret", "step-b5-f1-secret", "finance"} {
		require.NotContains(t, string(body), secret)
	}
	require.Contains(t, string(body), `{"id":"`+placeholder.ID+`","kind":"external","depth":0}`)

	// A second response keys its placeholders afresh.
	again, _ := w.confined(confinedTo("marketing"), "ol", "r-root")
	require.NotEqual(t, placeholder.ID, again.Downstream[0].(ExternalImpactNode).ID)

	// Unrestricted: the whole graph, the impact query verbatim.
	whole, err := NewWithDatabase(context.Background(), w.db).Impact("ol", "r-root", 0)
	require.NoError(t, err)
	require.Len(t, whole.Downstream, 2)
	require.Equal(t, "b5-f1-secret", whole.Downstream[0].JobAlias)
}

// Repeat runs of one foreign producer, interleaved with another's, collapse
// to one placeholder each, response-wide (A, B, A -> A, B).
func TestConfinedImpactDedupesPlaceholdersResponseWide(t *testing.T) {
	w := newLineageWorld(t)
	m1 := w.job("b5-dedupe-m1", "marketing")
	fa := w.job("b5-dedupe-fa", "finance")
	fb := w.job("b5-dedupe-fb", "finance")
	w.run(m1, "marketing", nil, []string{"d-root"})
	w.run(fa, "finance", []string{"d-root"}, []string{"d-a"})
	w.run(fb, "finance", []string{"d-root"}, []string{"d-b"})
	w.run(fa, "finance", []string{"d-root"}, []string{"d-a"})

	whole, err := NewWithDatabase(context.Background(), w.db).Impact("ol", "d-root", 0)
	require.NoError(t, err)
	require.Len(t, whole.Downstream, 3)
	require.Equal(t, []string{"d-a", "d-b", "d-a"}, []string{whole.Downstream[0].DatasetName, whole.Downstream[1].DatasetName, whole.Downstream[2].DatasetName})

	got, _ := w.confined(confinedTo("marketing"), "ol", "d-root")
	require.Len(t, got.Downstream, 2)
	require.NotEqual(t, got.Downstream[0].(ExternalImpactNode).ID, got.Downstream[1].(ExternalImpactNode).ID)
}

// A node whose job moved into marketing after a finance run wrote it is
// foreign to marketing: the run's persisted namespace must be readable too.
func TestConfinedImpactRequiresTheRunNamespace(t *testing.T) {
	w := newLineageWorld(t)
	m1 := w.job("b5-moved-m1", "marketing")
	moved := w.job("b5-moved-job", "marketing")
	w.run(m1, "marketing", nil, []string{"mv-root"})
	w.run(moved, "finance", []string{"mv-root"}, []string{"mv-out-secret"})

	got, body := w.confined(confinedTo("marketing"), "ol", "mv-root")
	require.Len(t, got.Downstream, 1)
	require.IsType(t, ExternalImpactNode{}, got.Downstream[0])
	require.NotContains(t, string(body), "mv-out-secret")
	require.NotContains(t, string(body), "b5-moved-job")
}

// A root the caller does not own answers exactly as a root nobody recorded:
// the same status and downstream, with the caller's own root echoed back.
// Reading (consuming) a dataset never grants it.
func TestConfinedImpactForeignRootLikeNonexistent(t *testing.T) {
	w := newLineageWorld(t)
	f1 := w.job("b5-root-f1", "finance")
	f2 := w.job("b5-root-f2", "finance")
	m1 := w.job("b5-root-m1", "marketing")
	ctx := confinedTo("marketing")

	before, beforeBody := w.confined(ctx, "ol", "fin-root")
	require.Empty(t, before.Downstream)

	w.run(f1, "finance", nil, []string{"fin-root"})
	w.run(f2, "finance", []string{"fin-root"}, []string{"fin-down"})
	// An allowed consumer of the foreign root does not unlock it.
	w.run(m1, "marketing", []string{"fin-root"}, []string{"mkt-down"})

	after, afterBody := w.confined(ctx, "ol", "fin-root")
	require.Equal(t, string(beforeBody), string(afterBody), "the foreign root reads exactly as it did before it existed")
	require.Equal(t, "ol", after.RootNamespace)
	require.Equal(t, "fin-root", after.RootName)
	require.Equal(t, `{"root_namespace":"ol","root_name":"fin-root","downstream":[]}`, string(afterBody))

	_, missing := w.confined(ctx, "ol", "nobody-recorded-this")
	require.Equal(t, `{"root_namespace":"ol","root_name":"nobody-recorded-this","downstream":[]}`, string(missing))

	// A finance viewer owns it.
	fin, _ := w.confined(confinedTo("finance"), "ol", "fin-root")
	require.NotEmpty(t, fin.Downstream)
}

// A declared root (the registry's empty namespace) is owned by its declared
// producers, not by whoever observed it.
func TestConfinedImpactDeclaredRootOwnedByDeclaredProducers(t *testing.T) {
	w := newLineageWorld(t)
	finProducer := w.job("b5-decl-fin", "finance")
	mktConsumer := w.job("b5-decl-mkt", "marketing")
	require.NoError(t, w.db.Create(&models.DatasetDeclaration{
		ID: uuid.New(), JobID: finProducer.ID, JobAlias: finProducer.Alias, StepName: "s", Name: "warehouse/decl", Direction: models.DatasetDirectionProduces,
	}).Error)
	// Even an output row from a marketing run does not override the
	// declaration.
	taskRun := w.run(mktConsumer, "marketing", nil, nil)
	require.NoError(t, w.db.Create(&models.LineageDataset{ID: uuid.New(), TaskRunID: taskRun, Namespace: "", Name: "warehouse/decl", Direction: "output", CreatedAt: time.Now().UTC()}).Error)

	got, _ := w.confined(confinedTo("marketing"), "", "warehouse/decl")
	require.Empty(t, got.Downstream)
	r := authz.NewRedactor(confinedTo("finance"))
	visible, err := rootVisible(context.Background(), w.db, r, "", "warehouse/decl")
	require.NoError(t, err)
	require.True(t, visible)
	r = authz.NewRedactor(confinedTo("marketing"))
	visible, err = rootVisible(context.Background(), w.db, r, "", "warehouse/decl")
	require.NoError(t, err)
	require.False(t, visible)
}

// The first attempt reads the root and its nodes and its COMMIT fails, so
// ConfinedImpact runs again. Whatever the retry finds replaces what the failed
// attempt built.
func TestConfinedImpactRetryKeepsNothingFromTheFailedAttempt(t *testing.T) {
	failure := errors.New("checkpoint in progress")
	world := func(t *testing.T) (*lineageWorld, models.Job) {
		w := newLineageWorld(t)
		producer := w.job("retry-producer", "marketing")
		w.run(producer, "marketing", nil, []string{"retry-root"})
		w.run(w.job("retry-consumer", "marketing"), "marketing", []string{"retry-root"}, []string{"retry-derived"})
		return w, producer
	}

	t.Run("an unchanged root is not doubled", func(t *testing.T) {
		w, _ := world(t)
		ctx := confinedTo("marketing")
		r := authz.NewRedactor(ctx)
		uncontended, err := NewWithDatabase(ctx, w.db).ConfinedImpact(r, "ol", "retry-root", 0)
		require.NoError(t, err)
		require.NotEmpty(t, uncontended.Downstream)

		conn, faults := dbfaults.FailCommits(t, w.db, failure, 1)
		got, err := NewWithDatabase(ctx, conn).ConfinedImpact(r, "ol", "retry-root", 0)

		require.NoError(t, err)
		require.Equal(t, 2, faults.Begins)
		require.Equal(t, uncontended, got)
	})

	// The no-existence-oracle rule: a root the caller may no longer read
	// answers as a root nobody recorded, even though an attempt once read it.
	t.Run("a root that became unreadable keeps no nodes", func(t *testing.T) {
		w, producer := world(t)
		conn, faults := dbfaults.FailCommits(t, w.db, failure, 1)
		faults.AfterRollback = func() {
			require.NoError(t, w.db.Model(&models.Job{}).Where("id = ?", producer.ID).Update("namespace", "finance").Error)
		}
		ctx := confinedTo("marketing")

		got, err := NewWithDatabase(ctx, conn).ConfinedImpact(authz.NewRedactor(ctx), "ol", "retry-root", 0)

		require.NoError(t, err)
		require.Equal(t, 2, faults.Begins)
		body, err := json.Marshal(got)
		require.NoError(t, err)
		require.Equal(t, `{"root_namespace":"ol","root_name":"retry-root","downstream":[]}`, string(body))
	})
}
