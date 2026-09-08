package diff

import (
	"bytes"
	"context"
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

	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// JobSpec captures the fields that participate in diffing.
type JobSpec struct {
	Alias       string            `json:"alias"`
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
	OnUpstreamHold string         `json:"onUpstreamHold,omitempty"`
	Trigger        TriggerSpec    `json:"trigger"`
	Callbacks      []CallbackSpec `json:"callbacks"`
	Steps          []StepSpec     `json:"steps"`
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
	Engine       string         `json:"engine"`
	Image        string         `json:"image"`
	Command      []string       `json:"command"`
	OutputSchema map[string]any `json:"outputSchema"`
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

// FromDefinition normalises a job definition into a JobSpec.
func FromDefinition(def *schema.Definition) JobSpec {
	return JobSpec{
		Alias:          def.Metadata.Alias,
		Labels:         cloneMap(def.Metadata.Labels),
		Annotations:    cloneMap(def.Metadata.Annotations),
		Remediation:    def.Metadata.Remediation,
		OnUpstreamHold: def.Metadata.OnUpstreamHold,
		Trigger: TriggerSpec{
			Type:          def.Trigger.Type,
			Configuration: cloneMap(def.Trigger.Configuration),
		},
		Callbacks: copyCallbacks(def.Callbacks),
		Steps:     copySteps(def.Steps),
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

// LoadDatabaseSpecs loads all jobs from the database into specs keyed by alias.
func LoadDatabaseSpecs(ctx context.Context, db *gorm.DB) (map[string]JobSpec, error) {
	var jobs []models.Job
	if err := db.WithContext(ctx).Find(&jobs).Error; err != nil {
		return nil, err
	}

	specs := make(map[string]JobSpec, len(jobs))
	for i := range jobs {
		job := &jobs[i]
		spec, err := buildJobSpec(ctx, db, job)
		if err != nil {
			return nil, err
		}
		specs[job.Alias] = spec
	}
	return specs, nil
}

func buildJobSpec(ctx context.Context, db *gorm.DB, job *models.Job) (JobSpec, error) {
	spec := JobSpec{
		Alias:          job.Alias,
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

	steps, err := loadSteps(ctx, db, job.ID)
	if err != nil {
		return JobSpec{}, err
	}
	spec.Steps = steps

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

func loadSteps(ctx context.Context, db *gorm.DB, jobID uuid.UUID) ([]StepSpec, error) {
	var tasks []models.Task
	if err := db.WithContext(ctx).
		Where("job_id = ?", jobID).
		Order("position asc").
		Order("created_at asc").
		Find(&tasks).Error; err != nil {
		return nil, err
	}
	if len(tasks) == 0 {
		return nil, nil
	}

	atomIDs := make([]uuid.UUID, 0, len(tasks))
	for _, task := range tasks {
		atomIDs = append(atomIDs, task.AtomID)
	}

	var atoms []models.Atom
	if err := db.WithContext(ctx).
		Where("id IN ?", atomIDs).
		Find(&atoms).Error; err != nil {
		return nil, err
	}
	atomByID := make(map[uuid.UUID]*models.Atom, len(atoms))
	for i := range atoms {
		atom := &atoms[i]
		atomByID[atom.ID] = atom
	}

	producesByStep, err := loadProducedDatasetSpecs(ctx, db, jobID)
	if err != nil {
		return nil, err
	}

	steps := make([]StepSpec, 0, len(tasks))
	for _, task := range tasks {
		atom := atomByID[task.AtomID]
		if atom == nil {
			return nil, fmt.Errorf("atom %s not found", task.AtomID)
		}
		outputSchema, err := parseJSONConfigBytes(task.OutputSchema)
		if err != nil {
			return nil, fmt.Errorf("task %s output_schema: %w", task.ID, err)
		}
		steps = append(steps, StepSpec{
			Engine:       string(atom.Engine),
			Image:        atom.Image,
			Command:      slices.Clone(atom.Cmd()),
			OutputSchema: outputSchema,
			Produces:     producesByStep[task.Name],
		})
	}
	return steps, nil
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
			if !isYAML(p) {
				return nil
			}
			return decodeDefinitions(p, fn)
		})
	}
	if !isYAML(path) {
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

func isYAML(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".yaml" || ext == ".yml"
}

func cloneMap[K comparable, V any](in map[K]V) map[K]V {
	if in == nil {
		return map[K]V{}
	}
	return maps.Clone(in)
}

func copyCallbacks(cbs []schema.Callback) []CallbackSpec {
	if len(cbs) == 0 {
		return nil
	}
	result := make([]CallbackSpec, 0, len(cbs))
	for _, cb := range cbs {
		result = append(result, CallbackSpec{
			Type:          cb.Type,
			Configuration: cloneMap(cb.Configuration),
		})
	}
	return result
}

func copySteps(steps []schema.Step) []StepSpec {
	if len(steps) == 0 {
		return nil
	}
	result := make([]StepSpec, 0, len(steps))
	for _, step := range steps {
		result = append(result, StepSpec{
			Engine:       step.Engine,
			Image:        step.Image,
			Command:      slices.Clone(step.Command),
			OutputSchema: cloneMap(step.OutputSchema),
			Produces:     producedDatasetSpecs(step.Datasets),
		})
	}
	return result
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
