package jobdef

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	APIVersionV1 = "v1"
	KindJob      = "Job"

	TriggerCron = "cron"
	TriggerHTTP = "http"

	CallbackNotification = "notification"

	EngineDocker     = "docker"
	EngineKubernetes = "kubernetes"
	EnginePodman     = "podman"
)

// Definition models the root job document.
type Definition struct {
	Schema     string     `yaml:"$schema,omitempty" json:"$schema,omitempty"`
	APIVersion string     `yaml:"apiVersion" json:"apiVersion"`
	Kind       string     `yaml:"kind" json:"kind"`
	Metadata   Metadata   `yaml:"metadata" json:"metadata"`
	Trigger    Trigger    `yaml:"trigger" json:"trigger"`
	Callbacks  []Callback `yaml:"callbacks,omitempty" json:"callbacks,omitempty"`
	Steps      []Step     `yaml:"steps" json:"steps"`
}

// Metadata contains descriptive data for the job.
type Metadata struct {
	Alias       string            `yaml:"alias" json:"alias"`
	Labels      map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty" json:"annotations,omitempty"`
}

// Trigger defines how the job is triggered.
type Trigger struct {
	Type          string         `yaml:"type" json:"type"`
	Configuration map[string]any `yaml:"configuration" json:"configuration"`
}

// Callback defines a job callback (notification, etc.).
type Callback struct {
	Type          string         `yaml:"type" json:"type"`
	Configuration map[string]any `yaml:"configuration" json:"configuration"`
}

// Step defines an execution step.
type Step struct {
	Name    string   `yaml:"name" json:"name"`
	Engine  string   `yaml:"engine,omitempty" json:"engine,omitempty"`
	Image   string   `yaml:"image" json:"image"`
	Command []string `yaml:"command,omitempty" json:"command,omitempty"`
	Next    string   `yaml:"next,omitempty" json:"next,omitempty"`
}

// UnmarshalYAML sets defaults while deserialising a step.
func (s *Step) UnmarshalYAML(value *yaml.Node) error {
	type rawStep Step
	rs := rawStep{Engine: EngineDocker}
	if err := value.Decode(&rs); err != nil {
		return err
	}
	*s = Step(rs)
	if s.Engine == "" {
		s.Engine = EngineDocker
	}
	return nil
}

// Parse parses YAML bytes into a Definition.
func Parse(data []byte) (*Definition, error) {
	var def Definition
	if err := yaml.Unmarshal(data, &def); err != nil {
		return nil, err
	}
	if err := def.Validate(); err != nil {
		return nil, err
	}
	return &def, nil
}

// Validate performs semantic validation on the definition.
func (d *Definition) Validate() error {
	if d.APIVersion != APIVersionV1 {
		return fmt.Errorf("unsupported apiVersion: %s", d.APIVersion)
	}
	if d.Kind != KindJob {
		return fmt.Errorf("unsupported kind: %s", d.Kind)
	}
	if strings.TrimSpace(d.Metadata.Alias) == "" {
		return fmt.Errorf("metadata.alias is required")
	}

	if err := validateTrigger(&d.Trigger); err != nil {
		return err
	}
	if err := validateCallbacks(d.Callbacks); err != nil {
		return err
	}
	if len(d.Steps) == 0 {
		return fmt.Errorf("steps must contain at least one entry")
	}
	if err := validateSteps(d.Steps); err != nil {
		return err
	}
	return nil
}

func validateTrigger(t *Trigger) error {
	switch t.Type {
	case TriggerCron, TriggerHTTP:
	default:
		return fmt.Errorf("trigger.type must be one of [%s,%s]", TriggerCron, TriggerHTTP)
	}
	if t.Configuration == nil {
		t.Configuration = map[string]any{}
	}
	return nil
}

func validateCallbacks(callbacks []Callback) error {
	for i, cb := range callbacks {
		if cb.Type != CallbackNotification {
			return fmt.Errorf("callbacks[%d].type must be %s", i, CallbackNotification)
		}
		if cb.Configuration == nil {
			return fmt.Errorf("callbacks[%d].configuration is required", i)
		}
	}
	return nil
}

func validateSteps(steps []Step) error {
	names := make(map[string]int, len(steps))
	for i := range steps {
		step := &steps[i]
		if strings.TrimSpace(step.Name) == "" {
			return fmt.Errorf("steps[%d].name is required", i)
		}
		if _, exists := names[step.Name]; exists {
			return fmt.Errorf("duplicate step name %q", step.Name)
		}
		names[step.Name] = i

		if strings.TrimSpace(step.Image) == "" {
			return fmt.Errorf("steps[%d].image is required", i)
		}
		switch step.Engine {
		case EngineDocker, EngineKubernetes, EnginePodman:
		default:
			return fmt.Errorf("steps[%d].engine must be one of [%s,%s,%s]", i, EngineDocker, EngineKubernetes, EnginePodman)
		}
	}

	for i, step := range steps {
		if step.Next == "" {
			continue
		}
		if _, exists := names[step.Next]; !exists {
			return fmt.Errorf("steps[%d].next references unknown step %q", i, step.Next)
		}
	}
	return nil
}
