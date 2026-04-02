package harness

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	internaljobdef "github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"gopkg.in/yaml.v3"
)

const (
	APIVersionV1 = "v1"
	KindHarness  = "Harness"
)

// File models a harness scenario manifest consumed by `caesium test --scenario`.
type File struct {
	APIVersion string     `yaml:"apiVersion"`
	Kind       string     `yaml:"kind"`
	Scenarios  []Scenario `yaml:"scenarios"`
}

// Scenario defines one executable harness case.
type Scenario struct {
	Name        string        `yaml:"name"`
	Path        string        `yaml:"path"`
	Alias       string        `yaml:"alias,omitempty"`
	MaxParallel int           `yaml:"maxParallel,omitempty"`
	TaskTimeout time.Duration `yaml:"taskTimeout,omitempty"`
	RunTimeout  time.Duration `yaml:"runTimeout,omitempty"`
	Expect      Expectation   `yaml:"expect"`
}

// Expectation describes the asserted outcome of a scenario run.
type Expectation struct {
	RunStatus     string              `yaml:"runStatus,omitempty"`
	ErrorContains string              `yaml:"errorContains,omitempty"`
	Tasks         []TaskExpectation   `yaml:"tasks,omitempty"`
	Metrics       []MetricExpectation `yaml:"metrics,omitempty"`
	Lineage       *LineageExpectation `yaml:"lineage,omitempty"`
}

// TaskExpectation describes assertions for one task.
type TaskExpectation struct {
	Name                 string            `yaml:"name"`
	Status               string            `yaml:"status,omitempty"`
	Output               map[string]string `yaml:"output,omitempty"`
	LogContains          []string          `yaml:"logContains,omitempty"`
	SchemaViolationCount *int              `yaml:"schemaViolationCount,omitempty"`
	CacheHit             *bool             `yaml:"cacheHit,omitempty"`
	ErrorContains        string            `yaml:"errorContains,omitempty"`
}

// MetricExpectation describes one Prometheus metric assertion.
type MetricExpectation struct {
	Name   string            `yaml:"name"`
	Labels map[string]string `yaml:"labels,omitempty"`
	Value  *float64          `yaml:"value,omitempty"`
	Delta  *float64          `yaml:"delta,omitempty"`
}

// LineageExpectation describes emitted OpenLineage assertions.
type LineageExpectation struct {
	TotalEvents *int           `yaml:"totalEvents,omitempty"`
	EventTypes  map[string]int `yaml:"eventTypes,omitempty"`
	JobNames    []string       `yaml:"jobNames,omitempty"`
}

// ResolvedScenario is a validated scenario with its source location.
type ResolvedScenario struct {
	Scenario   Scenario
	SourcePath string
}

// Definition loads and selects the job definition targeted by the scenario.
func (r ResolvedScenario) Definition() (*jobdef.Definition, error) {
	path := r.Scenario.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(filepath.Dir(r.SourcePath), path)
	}

	defs, err := internaljobdef.CollectDefinitions([]string{path}, true)
	if err != nil {
		return nil, err
	}
	if len(defs) == 0 {
		return nil, fmt.Errorf("scenario %s: no job definitions found at %s", r.Scenario.Name, path)
	}

	if strings.TrimSpace(r.Scenario.Alias) == "" {
		if len(defs) != 1 {
			return nil, fmt.Errorf("scenario %s: path %s contains %d job definitions; set scenario.alias", r.Scenario.Name, path, len(defs))
		}
		return &defs[0], nil
	}

	for i := range defs {
		if defs[i].Metadata.Alias == r.Scenario.Alias {
			return &defs[i], nil
		}
	}

	return nil, fmt.Errorf("scenario %s: alias %q not found in %s", r.Scenario.Name, r.Scenario.Alias, path)
}

// CollectScenarios loads harness scenarios from files or directories.
func CollectScenarios(paths []string) ([]ResolvedScenario, error) {
	if len(paths) == 0 {
		paths = []string{"."}
	}

	var scenarios []ResolvedScenario
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}

		if info.IsDir() {
			if err := filepath.WalkDir(p, func(path string, d os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if d.IsDir() || !isScenarioPath(path) {
					return nil
				}
				return appendScenarioFile(path, &scenarios)
			}); err != nil {
				return nil, err
			}
			continue
		}

		if !internaljobdef.IsYAML(p) {
			return nil, fmt.Errorf("%s is not a YAML file", p)
		}
		if err := appendScenarioFile(p, &scenarios); err != nil {
			return nil, err
		}
	}

	return scenarios, nil
}

func appendScenarioFile(path string, scenarios *[]ResolvedScenario) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var file File
		if err := dec.Decode(&file); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("%s: %w", path, err)
		}
		if isBlankFile(&file) {
			continue
		}
		if err := file.Validate(); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}

		for _, scenario := range file.Scenarios {
			*scenarios = append(*scenarios, ResolvedScenario{
				Scenario:   scenario,
				SourcePath: path,
			})
		}
	}

	return nil
}

func (f *File) Validate() error {
	if f.APIVersion != APIVersionV1 {
		return fmt.Errorf("unsupported apiVersion: %s", f.APIVersion)
	}
	if f.Kind != KindHarness {
		return fmt.Errorf("unsupported kind: %s", f.Kind)
	}
	if len(f.Scenarios) == 0 {
		return fmt.Errorf("scenarios must contain at least one entry")
	}

	seen := make(map[string]struct{}, len(f.Scenarios))
	for i := range f.Scenarios {
		if err := f.Scenarios[i].Validate(); err != nil {
			return fmt.Errorf("scenarios[%d]: %w", i, err)
		}
		if _, ok := seen[f.Scenarios[i].Name]; ok {
			return fmt.Errorf("duplicate scenario name %q", f.Scenarios[i].Name)
		}
		seen[f.Scenarios[i].Name] = struct{}{}
	}

	return nil
}

func (s *Scenario) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if strings.TrimSpace(s.Path) == "" {
		return fmt.Errorf("path is required")
	}
	if s.Expect.RunStatus == "" {
		s.Expect.RunStatus = "succeeded"
	}

	seen := make(map[string]struct{}, len(s.Expect.Tasks))
	for i := range s.Expect.Tasks {
		task := &s.Expect.Tasks[i]
		if strings.TrimSpace(task.Name) == "" {
			return fmt.Errorf("expect.tasks[%d].name is required", i)
		}
		if task.SchemaViolationCount != nil && *task.SchemaViolationCount < 0 {
			return fmt.Errorf("expect.tasks[%d].schemaViolationCount must be >= 0", i)
		}
		if _, ok := seen[task.Name]; ok {
			return fmt.Errorf("duplicate expected task %q", task.Name)
		}
		seen[task.Name] = struct{}{}
	}

	for i := range s.Expect.Metrics {
		metric := &s.Expect.Metrics[i]
		if strings.TrimSpace(metric.Name) == "" {
			return fmt.Errorf("expect.metrics[%d].name is required", i)
		}
		if metric.Value == nil && metric.Delta == nil {
			return fmt.Errorf("expect.metrics[%d] must set value or delta", i)
		}
	}

	if s.Expect.Lineage != nil {
		if s.Expect.Lineage.TotalEvents != nil && *s.Expect.Lineage.TotalEvents < 0 {
			return fmt.Errorf("expect.lineage.totalEvents must be >= 0")
		}
		for eventType, count := range s.Expect.Lineage.EventTypes {
			if strings.TrimSpace(eventType) == "" {
				return fmt.Errorf("expect.lineage.eventTypes contains an empty key")
			}
			if count < 0 {
				return fmt.Errorf("expect.lineage.eventTypes[%q] must be >= 0", eventType)
			}
		}
	}

	return nil
}

func isScenarioPath(path string) bool {
	lower := strings.ToLower(path)
	return strings.HasSuffix(lower, ".scenario.yaml") || strings.HasSuffix(lower, ".scenario.yml")
}

func isBlankFile(file *File) bool {
	if file == nil {
		return true
	}
	return strings.TrimSpace(file.APIVersion) == "" &&
		strings.TrimSpace(file.Kind) == "" &&
		len(file.Scenarios) == 0
}
