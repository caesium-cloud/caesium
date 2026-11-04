package diff

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/jobdef/yamlpath"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// JobSpec is the diff projection of one job. It covers every column job apply
// persists. Columns left out, and why, are listed in saved_columns_test.go.
// Literal environment values are never stored here: see EnvValue.
type JobSpec struct {
	Alias       string            `json:"alias"`
	Namespace   string            `json:"namespace"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	// Remediation participates in diffing because it is ENFORCED policy, not
	// documentation: it is what resolves the agent's effective playbook. A
	// remediation-only change that rendered as "no changes" would let an approver
	// wave through a policy edit believing nothing changed — and the same diff is
	// what an ApprovalRequest shows a human.
	Remediation *schema.MetadataRemediation `json:"remediation,omitempty"`
	// OnUpstreamHold participates for the same reason Remediation does: it is
	// ENFORCED admission policy (skip vs run when a consumed dataset is held),
	// so a flip that rendered as "no changes" would hide exactly the edit an
	// approver most needs to see.
	OnUpstreamHold   string              `json:"onUpstreamHold,omitempty"`
	MaxParallelTasks int                 `json:"maxParallelTasks,omitempty"`
	TaskTimeout      time.Duration       `json:"taskTimeout,omitempty"`
	RunTimeout       time.Duration       `json:"runTimeout,omitempty"`
	Priority         string              `json:"priority,omitempty"`
	Concurrency      *schema.Concurrency `json:"concurrency,omitempty"`
	RateLimits       []schema.RateLimit  `json:"rateLimits,omitempty"`
	SLA              *schema.SLAConfig   `json:"sla,omitempty"`
	SchemaValidation string              `json:"schemaValidation,omitempty"`
	ReplaySafe       bool                `json:"replaySafe,omitempty"`
	Cache            any                 `json:"cache,omitempty"`
	Trigger          TriggerSpec         `json:"trigger"`
	Callbacks        []CallbackSpec      `json:"callbacks"`
	Steps            []StepSpec          `json:"steps"`
	Edges            []EdgeSpec          `json:"edges,omitempty"`
	Declarations     []DeclarationSpec   `json:"declarations,omitempty"`
}

type TriggerSpec struct {
	Type          string         `json:"type"`
	Configuration map[string]any `json:"configuration"`
}

type CallbackSpec struct {
	Type          string         `json:"type"`
	Configuration map[string]any `json:"configuration"`
}

type StepSpec struct {
	Name         string            `json:"name"`
	Type         string            `json:"type,omitempty"`
	Engine       string            `json:"engine"`
	Image        string            `json:"image"`
	Command      []string          `json:"command"`
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	Retries      int               `json:"retries,omitempty"`
	RetryDelay   time.Duration     `json:"retryDelay,omitempty"`
	RetryBackoff bool              `json:"retryBackoff,omitempty"`
	TriggerRule  string            `json:"triggerRule,omitempty"`
	ReplaySafe   bool              `json:"replaySafe,omitempty"`
	RateLimit    RateLimitSpec     `json:"rateLimit,omitempty"`
	FanOut       *schema.FanOut    `json:"fanOut,omitempty"`
	Cache        any               `json:"cache,omitempty"`
	// OutputSchema keeps this Go name so a schema-only diff still says OutputSchema.
	OutputSchema any         `json:"outputSchema"`
	InputSchema  any         `json:"inputSchema,omitempty"`
	Runtime      RuntimeSpec `json:"runtime,omitempty"`
	// Produces carries the data circuit breaker's ENFORCED per-dataset policy.
	// `onViolation: warn → hold` is the difference between a logged note and a
	// broken circuit that skips every downstream consumer; leaving it out of the
	// diff would let that land as "no changes" in `caesium job diff` and in the
	// tier-3 ApprovalRequest diff alike.
	Produces []ProducedDatasetSpec `json:"produces,omitempty"`
}

// ProducedDatasetSpec is the diffable projection of one
// steps[].datasets.produces entry: its identity plus the assertion policy. The
// freshness SLO fields are deliberately absent — this struct exists to make the
// ENFORCEMENT policy visible, and adding unrelated fields here would change what
// every existing diff reports.
type ProducedDatasetSpec struct {
	Name        string                    `json:"name"`
	OnViolation string                    `json:"onViolation,omitempty"`
	Release     string                    `json:"release,omitempty"`
	Assertions  *schema.DatasetAssertions `json:"assertions,omitempty"`
}

// producedDatasetSpecs projects a step's declared produced datasets, sorted by
// name so a reordered manifest is not reported as a change.
func producedDatasetSpecs(datasets *schema.StepDatasets) []ProducedDatasetSpec {
	if datasets == nil || len(datasets.Produces) == 0 {
		return nil
	}
	out := make([]ProducedDatasetSpec, 0, len(datasets.Produces))
	for i := range datasets.Produces {
		p := &datasets.Produces[i]
		if p.Assertions.IsEmpty() && p.OnViolation == "" && p.Release == "" {
			// A dataset with no assertion policy carries nothing this struct is
			// for; including it would add noise to every pre-existing diff.
			continue
		}
		out = append(out, ProducedDatasetSpec{
			Name:        p.Name,
			OnViolation: p.OnViolation,
			Release:     p.Release,
			Assertions:  p.Assertions,
		})
	}
	if len(out) == 0 {
		return nil
	}
	slices.SortFunc(out, func(a, b ProducedDatasetSpec) int {
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// FromDefinition normalises a job definition into a JobSpec. Apply's defaults
// (type, engine, triggerRule, inherited replaySafe, sequential edges) are
// filled here so an unchanged manifest compares empty. A projection error is
// reported as an annotation rather than dropped, which would hide the change.
func FromDefinition(def *schema.Definition) JobSpec {
	spec, err := projectDefinition(def)
	if err == nil {
		return spec
	}
	alias := ""
	namespace := ""
	if def != nil {
		alias = def.Metadata.Alias
		namespace = models.NamespaceOrDefault(def.Metadata.Namespace)
	}
	return JobSpec{
		Alias:     alias,
		Namespace: namespace,
		Annotations: map[string]string{
			"caesium.dev/diff-projection-error": err.Error(),
		},
	}
}

// LoadDefinitions walks the provided paths collecting job definitions.
func LoadDefinitions(paths []string) (map[string]JobSpec, error) {
	if len(paths) == 0 {
		paths = []string{"."}
	}
	specs := make(map[string]JobSpec)
	for _, p := range paths {
		if err := collectPath(p, func(def *schema.Definition) error {
			alias := def.Metadata.Alias
			if _, exists := specs[alias]; exists {
				return fmt.Errorf("duplicate job alias %q", alias)
			}
			specs[alias] = FromDefinition(def)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return specs, nil
}

// RedactedTriggerConfiguration replaces, in a namespace-confined caller's
// baseline, the configuration of a trigger that also serves a job in a
// namespace the caller cannot read: reading a trigger needs the role in every
// job it serves (GET /v1/triggers/:id), and its configuration can carry that
// other namespace's webhook secret.
var RedactedTriggerConfiguration = map[string]any{"redacted": "the trigger also serves a job in another namespace"}

// LoadDatabaseSpecs loads all live jobs from the database into specs keyed by
// alias. When ctx carries a namespace restriction
// (internal/authz.WithAllowedNamespaces, set for a namespace-confined caller),
// only jobs in those namespaces are loaded and the configuration of a trigger
// shared with a job outside them is replaced by RedactedTriggerConfiguration.
//
// Every baseline, restricted or not, is one read-only snapshot, so steps,
// edges, declarations, and trigger configuration come from the same catalog
// state. A concurrent apply that commits a new step and an edge to it between
// those reads cannot fail the diff or return the edge without the step. A
// concurrent apply that moves a job out of a confined caller's namespaces and
// rewrites its trigger in place cannot pair a namespace check from before the
// move with a configuration from after it. Read-only, so the read/write split
// serves the snapshot from the read pool and it does not hold the catalog writer.
func LoadDatabaseSpecs(ctx context.Context, db *gorm.DB) (map[string]JobSpec, error) {
	var specs map[string]JobSpec
	err := ReadSnapshot(ctx, db, func(tx *gorm.DB) error {
		var err error
		specs, err = loadDatabaseSpecs(ctx, tx)
		return err
	})
	if err != nil {
		return nil, err
	}
	return specs, nil
}

// ReadSnapshot runs fn in one read-only transaction on conn, so every read fn
// makes through tx sees the same catalog state. Every diff baseline
// (LoadDatabaseSpecs) and a namespace-confined caller's lint read through it
// (the baseline here, the contract graph in api/rest/controller/jobdef).
//
// Read-only, so the read/write split serves it from the read pool and it never
// holds the single catalog writer (internal/run PredecessorExecutionInputs uses
// the same options). SQLite and dqlite keep one read view per transaction;
// PostgreSQL's READ COMMITTED would take a new one per statement.
//
// The pool's retry covers neither a transaction's statements nor its COMMIT,
// so the whole snapshot is retried on transient contention (db.TransactionOn:
// the shared budget, then the last error surfaces). fn can therefore run more
// than once, even after returning nil: COMMIT fails after fn has succeeded, so
// a complete attempt can still be discarded. The next attempt reads a newer
// snapshot, which may disagree with the last one. fn must keep nothing from an
// earlier attempt: assign its results whole on every path that returns nil, or
// reset them first, and never add to anything declared outside it. Only an
// error fn returns is retried. conn must not already be a transaction: fn's
// retry belongs to whoever opened that one.
func ReadSnapshot(ctx context.Context, conn *gorm.DB, fn func(tx *gorm.DB) error) error {
	options := &sql.TxOptions{ReadOnly: true}
	if conn.Name() == "postgres" {
		options.Isolation = sql.LevelRepeatableRead
	}
	return db.TransactionOn(ctx, conn, fn, options)
}

// afterBaselineJobsLoaded, when set (tests only), runs between loading the
// baseline's jobs and reading their triggers, where a concurrent apply would
// interleave.
var afterBaselineJobsLoaded func()

// afterBaselineStepsLoaded, when set (tests only), runs after the baseline's
// steps are read and before its edges, where a concurrent apply would commit
// a new step and an edge to it.
var afterBaselineStepsLoaded func()

func loadDatabaseSpecs(ctx context.Context, db *gorm.DB) (map[string]JobSpec, error) {
	var jobs []models.Job
	if err := authz.NamespaceFilter(db.WithContext(ctx), "namespace").Find(&jobs).Error; err != nil {
		return nil, err
	}
	if afterBaselineJobsLoaded != nil {
		afterBaselineJobsLoaded()
	}
	shared, err := foreignSharedTriggers(ctx, db, jobs)
	if err != nil {
		return nil, err
	}

	specs := make(map[string]JobSpec, len(jobs))
	for i := range jobs {
		job := &jobs[i]
		spec, err := buildJobSpec(ctx, db, job)
		if err != nil {
			return nil, err
		}
		if _, redact := shared[job.TriggerID]; redact {
			spec.Trigger.Configuration = maps.Clone(RedactedTriggerConfiguration)
		}
		specs[job.Alias] = spec
	}
	return specs, nil
}

// foreignSharedTriggers returns, for a restricted ctx, the triggers of jobs
// that also serve a live job outside the allowed namespaces; nil when ctx is
// unrestricted.
func foreignSharedTriggers(ctx context.Context, db *gorm.DB, jobs []models.Job) (map[uuid.UUID]struct{}, error) {
	allowed, restricted := authz.AllowedNamespaces(ctx)
	if !restricted || len(jobs) == 0 {
		return nil, nil
	}
	triggerIDs := make([]uuid.UUID, 0, len(jobs))
	for i := range jobs {
		triggerIDs = append(triggerIDs, jobs[i].TriggerID)
	}
	var peers []models.Job
	if err := db.WithContext(ctx).Select("id", "trigger_id", "namespace").Where("trigger_id IN ?", triggerIDs).Find(&peers).Error; err != nil {
		return nil, err
	}
	shared := make(map[uuid.UUID]struct{})
	for i := range peers {
		if !slices.Contains(allowed, models.NamespaceOrDefault(peers[i].Namespace)) {
			shared[peers[i].TriggerID] = struct{}{}
		}
	}
	return shared, nil
}

func buildJobSpec(ctx context.Context, db *gorm.DB, job *models.Job) (JobSpec, error) {
	spec := JobSpec{
		Alias:          job.Alias,
		Namespace:      models.NamespaceOrDefault(job.Namespace),
		Labels:         jsonMapToStringMap(job.Labels),
		Annotations:    jsonMapToStringMap(job.Annotations),
		OnUpstreamHold: job.OnUpstreamHold,
	}
	if len(job.Remediation) > 0 {
		var remediation schema.MetadataRemediation
		if err := json.Unmarshal(job.Remediation, &remediation); err != nil {
			return JobSpec{}, fmt.Errorf("job %s remediation: %w", job.ID, err)
		}
		spec.Remediation = &remediation
	}
	if err := applyStoredJobColumns(&spec, job); err != nil {
		return JobSpec{}, err
	}

	var trigger models.Trigger
	if err := db.WithContext(ctx).Where("id = ?", job.TriggerID).First(&trigger).Error; err != nil {
		return JobSpec{}, err
	}
	cfg, err := parseJSONConfig(trigger.Configuration)
	if err != nil {
		return JobSpec{}, fmt.Errorf("trigger %s configuration: %w", job.TriggerID, err)
	}
	spec.Trigger = TriggerSpec{
		Type:          string(trigger.Type),
		Configuration: cfg,
	}

	var callbacks models.Callbacks
	if err := db.WithContext(ctx).Where("job_id = ?", job.ID).Order("position asc").Order("created_at asc").Find(&callbacks).Error; err != nil {
		return JobSpec{}, err
	}
	spec.Callbacks = make([]CallbackSpec, 0, len(callbacks))
	for _, cb := range callbacks {
		cfg, err := parseJSONConfig(cb.Configuration)
		if err != nil {
			return JobSpec{}, fmt.Errorf("callback %s configuration: %w", cb.ID, err)
		}
		spec.Callbacks = append(spec.Callbacks, CallbackSpec{
			Type:          string(cb.Type),
			Configuration: cfg,
		})
	}

	steps, names, err := loadSteps(ctx, db, job.ID)
	if err != nil {
		return JobSpec{}, err
	}
	spec.Steps = steps
	if afterBaselineStepsLoaded != nil {
		afterBaselineStepsLoaded()
	}
	edges, err := loadEdges(ctx, db, job.ID, names)
	if err != nil {
		return JobSpec{}, err
	}
	spec.Edges = edges
	decls, err := loadDeclarations(ctx, db, job.ID)
	if err != nil {
		return JobSpec{}, err
	}
	spec.Declarations = decls

	return spec, nil
}

// loadProducedDatasetSpecs reads the persisted assertion policy off the declared
// registry (dataset_declarations), grouped by step name. The registry — not the
// stored manifest — is the server's own view of the policy, which is what a diff
// against the server must compare.
func loadProducedDatasetSpecs(ctx context.Context, db *gorm.DB, jobID uuid.UUID) (map[string][]ProducedDatasetSpec, error) {
	var decls []models.DatasetDeclaration
	if err := db.WithContext(ctx).
		Where("job_id = ? AND direction = ?", jobID, models.DatasetDirectionProduces).
		Order("name asc").
		Find(&decls).Error; err != nil {
		return nil, err
	}

	byStep := make(map[string][]ProducedDatasetSpec)
	for i := range decls {
		decl := &decls[i]
		if decl.AssertionsJSON == "" && decl.OnViolation == "" && decl.Release == "" {
			continue
		}
		spec := ProducedDatasetSpec{
			Name:        decl.Name,
			OnViolation: decl.OnViolation,
			Release:     decl.Release,
		}
		if decl.AssertionsJSON != "" {
			var assertions schema.DatasetAssertions
			if err := json.Unmarshal([]byte(decl.AssertionsJSON), &assertions); err != nil {
				return nil, fmt.Errorf("dataset declaration %s assertions: %w", decl.ID, err)
			}
			spec.Assertions = &assertions
		}
		byStep[decl.StepName] = append(byStep[decl.StepName], spec)
	}
	return byStep, nil
}

func loadSteps(ctx context.Context, db *gorm.DB, jobID uuid.UUID) ([]StepSpec, map[uuid.UUID]string, error) {
	var tasks []models.Task
	if err := db.WithContext(ctx).
		Where("job_id = ?", jobID).
		Order("position asc").
		Order("created_at asc").
		Find(&tasks).Error; err != nil {
		return nil, nil, err
	}
	if len(tasks) == 0 {
		return nil, nil, nil
	}

	atomIDs := make([]uuid.UUID, 0, len(tasks))
	for _, task := range tasks {
		atomIDs = append(atomIDs, task.AtomID)
	}

	var atoms []models.Atom
	if err := db.WithContext(ctx).
		Where("id IN ?", atomIDs).
		Find(&atoms).Error; err != nil {
		return nil, nil, err
	}
	atomByID := make(map[uuid.UUID]*models.Atom, len(atoms))
	for i := range atoms {
		atom := &atoms[i]
		atomByID[atom.ID] = atom
	}

	producesByStep, err := loadProducedDatasetSpecs(ctx, db, jobID)
	if err != nil {
		return nil, nil, err
	}

	steps := make([]StepSpec, 0, len(tasks))
	names := make(map[uuid.UUID]string, len(tasks))
	for i := range tasks {
		task := &tasks[i]
		atom := atomByID[task.AtomID]
		if atom == nil {
			return nil, nil, fmt.Errorf("atom %s not found", task.AtomID)
		}
		step, err := stepFromStored(task, atom, producesByStep[task.Name])
		if err != nil {
			return nil, nil, err
		}
		names[task.ID] = task.Name
		steps = append(steps, step)
	}
	return steps, names, nil
}

func collectPath(path string, fn func(*schema.Definition) error) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return filepath.WalkDir(path, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			if !yamlpath.IsYAML(p) {
				return nil
			}
			return decodeDefinitions(p, fn)
		})
	}
	if !yamlpath.IsYAML(path) {
		return fmt.Errorf("%s is not a YAML file", path)
	}
	return decodeDefinitions(path, fn)
}

func decodeDefinitions(path string, fn func(*schema.Definition) error) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var def schema.Definition
		if err := dec.Decode(&def); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("%s: %w", path, err)
		}
		if isBlankDefinition(&def) {
			continue
		}
		if err := def.Validate(); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if err := fn(&def); err != nil {
			return err
		}
	}
	return nil
}

func cloneMap[K comparable, V any](in map[K]V) map[K]V {
	if in == nil {
		return map[K]V{}
	}
	return maps.Clone(in)
}

func parseJSONConfig(raw string) (map[string]any, error) {
	return parseJSONConfigBytes([]byte(raw))
}

func parseJSONConfigBytes(raw []byte) (map[string]any, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return map[string]any{}, nil
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func jsonMapToStringMap(in datatypes.JSONMap) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		switch val := v.(type) {
		case string:
			out[k] = val
		default:
			out[k] = fmt.Sprint(val)
		}
	}
	return out
}

func isBlankDefinition(def *schema.Definition) bool {
	if def == nil {
		return true
	}
	if strings.TrimSpace(def.Metadata.Alias) != "" {
		return false
	}
	if def.APIVersion != "" || def.Kind != "" || def.Trigger.Type != "" {
		return false
	}
	return len(def.Steps) == 0 && len(def.Callbacks) == 0
}
