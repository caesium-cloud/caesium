package diff

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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
	Alias       string
	Labels      map[string]string
	Annotations map[string]string
	Trigger     TriggerSpec
	Callbacks   []CallbackSpec
	Steps       []StepSpec
}

type TriggerSpec struct {
	Type          string
	Configuration map[string]any
}

type CallbackSpec struct {
	Type          string
	Configuration map[string]any
}

type StepSpec struct {
	Engine  string
	Image   string
	Command []string
}

// FromDefinition normalises a job definition into a JobSpec.
func FromDefinition(def *schema.Definition) JobSpec {
	return JobSpec{
		Alias:       def.Metadata.Alias,
		Labels:      cloneStringMap(def.Metadata.Labels),
		Annotations: cloneStringMap(def.Metadata.Annotations),
		Trigger: TriggerSpec{
			Type:          def.Trigger.Type,
			Configuration: cloneAnyMap(def.Trigger.Configuration),
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
		Alias:       job.Alias,
		Labels:      jsonMapToStringMap(job.Labels),
		Annotations: jsonMapToStringMap(job.Annotations),
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
	if err := db.WithContext(ctx).Where("job_id = ?", job.ID).Order("created_at asc").Find(&callbacks).Error; err != nil {
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

func loadSteps(ctx context.Context, db *gorm.DB, jobID uuid.UUID) ([]StepSpec, error) {
	var tasks []models.Task
	if err := db.WithContext(ctx).
		Where("job_id = ?", jobID).
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

	steps := make([]StepSpec, 0, len(tasks))
	for _, task := range tasks {
		atom := atomByID[task.AtomID]
		if atom == nil {
			return nil, fmt.Errorf("atom %s not found", task.AtomID)
		}
		steps = append(steps, StepSpec{
			Engine:  string(atom.Engine),
			Image:   atom.Image,
			Command: append([]string(nil), atom.Cmd()...),
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

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneAnyMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyCallbacks(cbs []schema.Callback) []CallbackSpec {
	if len(cbs) == 0 {
		return nil
	}
	result := make([]CallbackSpec, 0, len(cbs))
	for _, cb := range cbs {
		result = append(result, CallbackSpec{
			Type:          cb.Type,
			Configuration: cloneAnyMap(cb.Configuration),
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
			Engine:  step.Engine,
			Image:   step.Image,
			Command: append([]string(nil), step.Command...),
		})
	}
	return result
}

func parseJSONConfig(raw string) (map[string]any, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}, nil
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
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
