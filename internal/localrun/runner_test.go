package localrun

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/require"
)

// --- fakes ------------------------------------------------------------------
//
// internal/job (which Runner.RunWithResult drives via job.New) dispatches
// steps through a real atom.Engine by default. These fakes let a local-run
// test exercise the real execution path — Create, Wait, Logs — without Docker,
// resolving every atom immediately so the run completes synchronously.

type fakeLocalAtom struct {
	id       string
	result   atom.Result
	exitCode *int
}

func (f *fakeLocalAtom) ID() string           { return f.id }
func (f *fakeLocalAtom) State() atom.State    { return atom.Stopped }
func (f *fakeLocalAtom) Result() atom.Result  { return f.result }
func (f *fakeLocalAtom) ExitCode() *int       { return f.exitCode }
func (f *fakeLocalAtom) CreatedAt() time.Time { return time.Now() }
func (f *fakeLocalAtom) StartedAt() time.Time { return time.Now() }
func (f *fakeLocalAtom) StoppedAt() time.Time { return time.Now() }
func (f *fakeLocalAtom) Engine() atom.Engine  { return nil }

// fakeLocalEngine is a minimal atom.Engine. Both the emitted result and the
// "printed" log are keyed off the CAESIUM_PARTITION env var the local
// executor injects into a fanned instance's container; an unfanned step
// carries no partition value and falls back to defaultKey.
type fakeLocalEngine struct {
	mu sync.Mutex

	resultByPartition   map[string]atom.Result
	logsByPartition     map[string]string
	exitCodeByPartition map[string]*int
	partitionByAtomID   map[string]string
}

const defaultKey = "__unfanned__"

func newFakeLocalEngine() *fakeLocalEngine {
	return &fakeLocalEngine{
		resultByPartition:   map[string]atom.Result{},
		logsByPartition:     map[string]string{},
		exitCodeByPartition: map[string]*int{},
		partitionByAtomID:   map[string]string{},
	}
}

func (e *fakeLocalEngine) Get(req *atom.EngineGetRequest) (atom.Atom, error) {
	return e.resolve(req.ID), nil
}

func (e *fakeLocalEngine) List(*atom.EngineListRequest) ([]atom.Atom, error) { return nil, nil }

func (e *fakeLocalEngine) Create(req *atom.EngineCreateRequest) (atom.Atom, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	partition := defaultKey
	if req.Spec.Env != nil {
		if p := req.Spec.Env[schema.DefaultFanOutEnv]; p != "" {
			partition = p
		}
	}
	id := "atom-" + req.Name
	e.partitionByAtomID[id] = partition
	return e.resolveLocked(id), nil
}

func (e *fakeLocalEngine) Wait(req *atom.EngineWaitRequest) (atom.Atom, error) {
	return e.resolve(req.ID), nil
}

func (e *fakeLocalEngine) Stop(*atom.EngineStopRequest) error { return nil }

func (e *fakeLocalEngine) Logs(req *atom.EngineLogsRequest) (io.ReadCloser, error) {
	e.mu.Lock()
	partition := e.partitionByAtomID[req.ID]
	logs := e.logsByPartition[partition]
	e.mu.Unlock()
	return io.NopCloser(strings.NewReader(logs)), nil
}

func (e *fakeLocalEngine) resolve(id string) atom.Atom {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.resolveLocked(id)
}

func (e *fakeLocalEngine) resolveLocked(id string) atom.Atom {
	partition := e.partitionByAtomID[id]
	result := e.resultByPartition[partition]
	if result == "" {
		result = atom.Success
	}
	return &fakeLocalAtom{id: id, result: result, exitCode: e.exitCodeByPartition[partition]}
}

// --- fixture ------------------------------------------------------------------

// fannedDefinition builds a two-step job: "list" emits a partitions marker,
// "process" fans out over it with failurePolicy=continue, so a failed
// partition does not cancel its still-pending siblings — every instance
// reaches its OWN distinct terminal status.
func fannedDefinition(alias string) *schema.Definition {
	return &schema.Definition{
		APIVersion: schema.APIVersionV1,
		Kind:       schema.KindJob,
		Metadata:   schema.Metadata{Alias: alias},
		Trigger: schema.Trigger{
			Type:          "cron",
			Configuration: map[string]any{"cron": "0 * * * *"},
		},
		Steps: []schema.Step{
			{
				Name:    "list",
				Type:    "task",
				Engine:  "docker",
				Image:   "alpine:3.23",
				Command: []string{"sh", "-c", "true"},
				Next:    []string{"process"},
			},
			{
				Name:      "process",
				Type:      "task",
				Engine:    "docker",
				Image:     "alpine:3.23",
				Command:   []string{"sh", "-c", "true"},
				DependsOn: []string{"list"},
				FanOut: &schema.FanOut{
					From:          "list",
					Env:           "CAESIUM_PARTITION",
					MaxPartitions: 16,
					FailurePolicy: "continue",
				},
			},
		},
	}
}

// --- tests --------------------------------------------------------------------

// TestLocalRunFannedGroupOrderedByPartitionIndexEachInstanceDistinct is the F9
// coverage for internal/localrun/runner.go's collectRunResult: a fanned step
// has N TaskRun rows under one catalog task, and the collected result must
// report every instance's OWN status and log — never an arbitrary sibling's —
// in partition_index order (the emission order "a","b","c", not dispatch or
// insertion order). The failed partition sits in the MIDDLE, so a result that
// silently collapsed to one row, or mixed up which instance's log belongs to
// which status, fails this test for a different reason than an ordering bug
// would.
func TestLocalRunFannedGroupOrderedByPartitionIndexEachInstanceDistinct(t *testing.T) {
	engine := newFakeLocalEngine()
	engine.resultByPartition["b"] = atom.Failure
	engine.logsByPartition["a"] = "log for partition a"
	engine.logsByPartition["b"] = "log for partition b: boom"
	engine.logsByPartition["c"] = "log for partition c"
	engine.logsByPartition[defaultKey] = `##caesium::partitions ["a","b","c"]`

	runner := New(Config{
		EngineFactory: func(context.Context) atom.Engine { return engine },
	})

	// A failed partition surfaces as a run error even under failurePolicy:
	// continue — RunWithResult still returns the collected result alongside
	// it, which is what this test needs to inspect.
	result, err := runner.RunWithResult(context.Background(), fannedDefinition("localrun-fanout-order"))
	require.Error(t, err)
	require.Contains(t, err.Error(), `partition "b" failed`)
	require.NotNil(t, result)

	var instances []TaskResult
	for _, tr := range result.Tasks {
		if tr.Name == "list" {
			continue
		}
		instances = append(instances, tr)
	}
	require.Len(t, instances, 3, "all three fan-out instances must be reported")

	// partition_index order is emission order: a, b, c.
	require.Equal(t, []string{"a", "b", "c"}, []string{instances[0].Partition, instances[1].Partition, instances[2].Partition},
		"instances must be reported in partition_index order, not insertion/dispatch order")

	require.Equal(t, "succeeded", instances[0].Status)
	require.Equal(t, "log for partition a", instances[0].LogText)

	require.Equal(t, "failed", instances[1].Status,
		"the middle partition must be reported failed, not shadowed by a succeeded sibling")
	require.Equal(t, "log for partition b: boom", instances[1].LogText,
		"the failed instance's OWN log must be attached, not a neighbour's")

	require.Equal(t, "succeeded", instances[2].Status)
	require.Equal(t, "log for partition c", instances[2].LogText)

	// Runner.Run is what `caesium dev` prints. It must name the failed
	// partition and show that instance's log, not a sibling's.
	wrapped := formatRunFailure(err, result)
	require.Error(t, wrapped)
	require.Contains(t, wrapped.Error(), "step process[b] attempt 1")
	require.Contains(t, wrapped.Error(), "log for partition b: boom")
	require.NotContains(t, wrapped.Error(), "log for partition a")
	require.NotContains(t, wrapped.Error(), "log for partition c")
}

// simpleDefinition builds a minimal one-step job declaring the given engine
// kind — P3-6 coverage for Config.EngineFactory.
func simpleDefinition(alias, engineKind string) *schema.Definition {
	return &schema.Definition{
		APIVersion: schema.APIVersionV1,
		Kind:       schema.KindJob,
		Metadata:   schema.Metadata{Alias: alias},
		Trigger: schema.Trigger{
			Type:          "cron",
			Configuration: map[string]any{"cron": "0 * * * *"},
		},
		Steps: []schema.Step{
			{
				Name:    "solo",
				Type:    "task",
				Engine:  engineKind,
				Image:   "alpine:3.23",
				Command: []string{"sh", "-c", "true"},
			},
		},
	}
}

// TestLocalRunEngineFactoryOverridesEveryEngineKind is the P3-6 coverage:
// Config.EngineFactory must be wired into every engine kind job.New supports
// (docker, kubernetes, podman), not just docker — otherwise a fixture whose
// step declares `engine: podman` or `engine: kubernetes` would silently fall
// through to job.New's REAL default engine for that kind instead of the
// injected fake, defeating the whole point of the hook.
func TestLocalRunEngineFactoryOverridesEveryEngineKind(t *testing.T) {
	for _, engineKind := range []string{"docker", "podman", "kubernetes"} {
		t.Run(engineKind, func(t *testing.T) {
			engine := newFakeLocalEngine()
			runner := New(Config{
				EngineFactory: func(context.Context) atom.Engine { return engine },
			})

			result, err := runner.RunWithResult(context.Background(), simpleDefinition("localrun-engine-"+engineKind, engineKind))
			require.NoError(t, err,
				"a %s step must resolve through the injected fake engine, not a real runtime", engineKind)
			require.Equal(t, "succeeded", result.Status)
			require.Len(t, result.Tasks, 1)
			require.Equal(t, "succeeded", result.Tasks[0].Status)
		})
	}
}

func (e *fakeLocalEngine) Stats(*atom.EngineStatsRequest) (atom.ResourceStats, error) {
	return atom.ResourceStats{}, atom.ErrStatsUnavailable
}

// TestRunFailureIncludesStepAttemptExitCodeAndLogTail is the dev-failure
// contract: Runner.Run (what `caesium dev` prints) names the step, the
// attempt, the raw exit code, and the captured stdout/stderr markers.
// RunWithResult keeps the executor error and returns the structured result.
func TestRunFailureIncludesStepAttemptExitCodeAndLogTail(t *testing.T) {
	const (
		step         = "extract"
		stdoutMarker = "DX_STDOUT_MISSING_COLUMN"
		stderrMarker = "DX_STDERR_CUSTOMER_ID_REQUIRED"
	)
	engine := newFakeLocalEngine()
	exitCode := 7
	engine.resultByPartition[defaultKey] = atom.Unknown
	engine.exitCodeByPartition[defaultKey] = &exitCode
	engine.logsByPartition[defaultKey] = stdoutMarker + "\n" + stderrMarker + "\n"

	runner := New(Config{
		EngineFactory: func(context.Context) atom.Engine { return engine },
	})
	def := simpleDefinition("dx-failure", "docker")
	def.Steps[0].Name = step

	result, err := runner.RunWithResult(context.Background(), def)
	require.Error(t, err)
	require.Contains(t, err.Error(), `failed with result "unknown"`)
	require.Contains(t, err.Error(), step)
	require.NotContains(t, err.Error(), stdoutMarker)
	require.NotContains(t, err.Error(), stderrMarker)
	require.NotNil(t, result)
	require.Len(t, result.Tasks, 1)
	require.Equal(t, step, result.Tasks[0].Name)
	require.Equal(t, stdoutMarker+"\n"+stderrMarker+"\n", result.Tasks[0].LogText)
	require.NotNil(t, result.Tasks[0].ExitCode)
	require.Equal(t, 7, *result.Tasks[0].ExitCode)
	require.Equal(t, 1, result.Tasks[0].Attempt)
	require.Equal(t, "failed", result.Tasks[0].Status)

	runErr := runner.Run(context.Background(), def)
	require.Error(t, runErr)
	msg := runErr.Error()
	require.Contains(t, msg, step)
	require.Contains(t, msg, "attempt 1")
	require.Contains(t, msg, "exit 7")
	require.Contains(t, msg, stdoutMarker)
	require.Contains(t, msg, stderrMarker)
	require.Contains(t, msg, `failed with result "unknown"`)
}
