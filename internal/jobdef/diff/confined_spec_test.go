package diff

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// TestLoadDatabaseSpecsConfinedBaseline: a context restricted to marketing
// (a namespace-confined diff, identity-and-access B2) loads only marketing's
// jobs and redacts the configuration of a trigger shared with a finance job;
// an unrestricted context loads every job unchanged.
func TestLoadDatabaseSpecsConfinedBaseline(t *testing.T) {
	db := openDiffTestDB(t)
	defer closeDiffTestDB(db)
	definition := func(alias, namespace string, trigger map[string]any) schema.Definition {
		return schema.Definition{
			Metadata: schema.Metadata{Alias: alias, Namespace: namespace},
			Trigger:  schema.Trigger{Type: schema.TriggerHTTP, Configuration: trigger},
			Steps:    []schema.Step{{Name: "task", Engine: schema.EngineDocker, Image: "alpine:3.23", Command: []string{"true"}}},
		}
	}
	insertDiffDefinition(t, db, definition("mkt-shared", "marketing", map[string]any{"path": "/hooks/shared", "secret": "finance-secret"}))
	insertDiffDefinition(t, db, definition("mkt-own", "marketing", map[string]any{"path": "/hooks/own", "secret": "marketing-secret"}))
	insertDiffDefinition(t, db, definition("fin", "finance", map[string]any{"path": "/hooks/fin"}))
	var shared models.Job
	require.NoError(t, db.Where("alias = ?", "mkt-shared").First(&shared).Error)
	require.NoError(t, db.Model(&models.Job{}).Where("alias = ?", "fin").Update("trigger_id", shared.TriggerID).Error)

	all, err := LoadDatabaseSpecs(context.Background(), db)
	require.NoError(t, err)
	require.Len(t, all, 3)
	require.Equal(t, "finance-secret", all["mkt-shared"].Trigger.Configuration["secret"], "unrestricted: unchanged")

	confined, err := LoadDatabaseSpecs(authz.WithAllowedNamespaces(context.Background(), []string{"marketing"}), db)
	require.NoError(t, err)
	require.Len(t, confined, 2)
	require.NotContains(t, confined, "fin")
	require.Equal(t, RedactedTriggerConfiguration, confined["mkt-shared"].Trigger.Configuration)
	require.Equal(t, "marketing-secret", confined["mkt-own"].Trigger.Configuration["secret"], "a trigger only marketing's job uses is readable")

	both, err := LoadDatabaseSpecs(authz.WithAllowedNamespaces(context.Background(), []string{"marketing", "finance"}), db)
	require.NoError(t, err)
	require.Len(t, both, 3)
	require.Equal(t, "finance-secret", both["mkt-shared"].Trigger.Configuration["secret"])

	none, err := LoadDatabaseSpecs(authz.WithAllowedNamespaces(context.Background(), nil), db)
	require.NoError(t, err)
	require.Empty(t, none, "restricted to nothing loads nothing")
}

// TestLoadDatabaseSpecsConfinedBaselineIsOneSnapshot: an unrestricted apply
// that moves a marketing job to finance and rewrites its trigger in place,
// landing between the confined baseline's job list and its trigger reads,
// must not hand the new (finance) trigger secret to the marketing caller.
func TestLoadDatabaseSpecsConfinedBaselineIsOneSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	dsn := "file:" + path + "?_journal_mode=WAL&_busy_timeout=5000"
	reader, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	defer closeDiffTestDB(reader)
	require.NoError(t, reader.AutoMigrate(&models.Trigger{}, &models.Job{}, &models.Atom{}, &models.Task{}, &models.Callback{}, &models.TaskEdge{}, &models.DatasetDeclaration{}))
	writer, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	defer closeDiffTestDB(writer)

	insertDiffDefinition(t, reader, schema.Definition{
		Metadata: schema.Metadata{Alias: "mkt", Namespace: "marketing"},
		Trigger:  schema.Trigger{Type: schema.TriggerHTTP, Configuration: map[string]any{"path": "/hooks/mkt", "secret": "marketing-secret"}},
		Steps:    []schema.Step{{Name: "task", Engine: schema.EngineDocker, Image: "alpine:3.23", Command: []string{"true"}}},
	})
	var job models.Job
	require.NoError(t, reader.Where("alias = ?", "mkt").First(&job).Error)

	moved := false
	afterBaselineJobsLoaded = func() {
		if moved {
			return
		}
		moved = true
		require.NoError(t, writer.Exec(`UPDATE jobs SET namespace = 'finance' WHERE id = ?`, job.ID).Error)
		require.NoError(t, writer.Exec(`UPDATE triggers SET configuration = ? WHERE id = ?`,
			`{"path":"/hooks/mkt","secret":"finance-secret"}`, job.TriggerID).Error)
	}
	t.Cleanup(func() { afterBaselineJobsLoaded = nil })

	specs, err := LoadDatabaseSpecs(authz.WithAllowedNamespaces(context.Background(), []string{"marketing"}), reader)
	require.NoError(t, err)
	require.True(t, moved, "the concurrent apply ran mid-read")
	require.Contains(t, specs, "mkt", "the snapshot still holds the job in marketing")
	require.Equal(t, "marketing-secret", specs["mkt"].Trigger.Configuration["secret"],
		"the trigger is read from the same snapshot as the namespace check")

	after, err := LoadDatabaseSpecs(authz.WithAllowedNamespaces(context.Background(), []string{"marketing"}), reader)
	require.NoError(t, err)
	require.NotContains(t, after, "mkt", "once moved, the job leaves the marketing baseline")
}

// recordingPool records the options every transaction is begun with.
type recordingPool struct {
	*sql.DB
	begun []*sql.TxOptions
}

func (p *recordingPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	p.begun = append(p.begun, opts)
	return p.DB.BeginTx(ctx, opts)
}

// TestLoadDatabaseSpecsConfinedSnapshotIsReadOnly: each baseline, unrestricted
// or confined, begins one read-only snapshot. Read-only is what routes it to
// the read pool of the catalog's read/write split
// (pkg/db TestRWSplitReadOnlyTransactionDoesNotWaitForSerializedWriter)
// instead of holding the single writer for the whole baseline.
func TestLoadDatabaseSpecsConfinedSnapshotIsReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	raw, err := sql.Open("sqlite3", "file:"+path+"?_journal_mode=WAL&_busy_timeout=5000")
	require.NoError(t, err)
	defer func() { _ = raw.Close() }()
	pool := &recordingPool{DB: raw}
	db, err := gorm.Open(sqlite.Dialector{Conn: pool}, &gorm.Config{SkipDefaultTransaction: true})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.Trigger{}, &models.Job{}, &models.Atom{}, &models.Task{}, &models.Callback{}, &models.TaskEdge{}, &models.DatasetDeclaration{}))

	_, err = LoadDatabaseSpecs(context.Background(), db)
	require.NoError(t, err)
	require.Len(t, pool.begun, 1, "an unrestricted baseline begins one transaction")
	require.NotNil(t, pool.begun[0])
	require.True(t, pool.begun[0].ReadOnly)

	_, err = LoadDatabaseSpecs(authz.WithAllowedNamespaces(context.Background(), []string{"marketing"}), db)
	require.NoError(t, err)
	require.Len(t, pool.begun, 2, "a confined baseline begins one more transaction")
	require.NotNil(t, pool.begun[1])
	require.True(t, pool.begun[1].ReadOnly)
}

// TestLoadDatabaseSpecsEdgeReadSharesTheStepSnapshot: an unrestricted load
// reads steps and edges in one snapshot. A second connection that commits a
// new task and an edge to it between those reads must not fail the load, and
// the returned spec must not contain that edge without its step.
func TestLoadDatabaseSpecsEdgeReadSharesTheStepSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	dsn := "file:" + path + "?_journal_mode=WAL&_busy_timeout=5000"
	reader, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	defer closeDiffTestDB(reader)
	require.NoError(t, reader.AutoMigrate(&models.Trigger{}, &models.Job{}, &models.Atom{}, &models.Task{}, &models.Callback{}, &models.TaskEdge{}, &models.DatasetDeclaration{}))
	writer, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	defer closeDiffTestDB(writer)

	insertDiffDefinition(t, reader, schema.Definition{
		Metadata: schema.Metadata{Alias: "pipeline"},
		Trigger:  schema.Trigger{Type: schema.TriggerHTTP, Configuration: map[string]any{"path": "/hooks/pipeline"}},
		Steps: []schema.Step{
			{Name: "a", Engine: schema.EngineDocker, Image: "alpine:3.23", Command: []string{"true"}, Next: []string{"b"}},
			{Name: "b", Engine: schema.EngineDocker, Image: "alpine:3.23", Command: []string{"true"}},
		},
	})
	var job models.Job
	require.NoError(t, reader.Where("alias = ?", "pipeline").First(&job).Error)
	var from models.Task
	require.NoError(t, reader.Where("job_id = ? AND name = ?", job.ID, "a").First(&from).Error)

	inserted := false
	afterBaselineStepsLoaded = func() {
		if inserted {
			return
		}
		inserted = true
		now := time.Now().UTC()
		atomID := uuid.New()
		require.NoError(t, writer.Create(&models.Atom{
			ID:        atomID,
			Engine:    models.AtomEngineDocker,
			Image:     "alpine:3.23",
			Command:   `["true"]`,
			CreatedAt: now,
			UpdatedAt: now,
		}).Error)
		taskID := uuid.New()
		require.NoError(t, writer.Create(&models.Task{
			ID:          taskID,
			JobID:       job.ID,
			AtomID:      atomID,
			Name:        "c",
			Position:    2,
			Type:        schema.StepTypeTask,
			TriggerRule: schema.TriggerRuleAllSuccess,
			CreatedAt:   now,
			UpdatedAt:   now,
		}).Error)
		require.NoError(t, writer.Create(&models.TaskEdge{
			ID:         uuid.New(),
			JobID:      job.ID,
			FromTaskID: from.ID,
			ToTaskID:   taskID,
			CreatedAt:  now,
			UpdatedAt:  now,
		}).Error)
	}
	t.Cleanup(func() { afterBaselineStepsLoaded = nil })

	specs, err := LoadDatabaseSpecs(context.Background(), reader)
	require.NoError(t, err)
	require.True(t, inserted, "the concurrent apply ran between the step and edge reads")
	spec, ok := specs["pipeline"]
	require.True(t, ok)
	names := make([]string, 0, len(spec.Steps))
	present := make(map[string]struct{}, len(spec.Steps))
	for _, step := range spec.Steps {
		names = append(names, step.Name)
		present[step.Name] = struct{}{}
	}
	require.ElementsMatch(t, []string{"a", "b"}, names)
	require.Equal(t, []EdgeSpec{{From: "a", To: "b"}}, spec.Edges)
	for _, edge := range spec.Edges {
		_, fromOK := present[edge.From]
		_, toOK := present[edge.To]
		require.Truef(t, fromOK && toOK, "edge %s -> %s has no step in this snapshot", edge.From, edge.To)
	}
}
