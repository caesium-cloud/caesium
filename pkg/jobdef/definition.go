package jobdef

import (
	"fmt"
	"sort"
	"strings"

	"github.com/caesium-cloud/caesium/pkg/container"
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
	Name           string   `yaml:"name" json:"name"`
	Engine         string   `yaml:"engine,omitempty" json:"engine,omitempty"`
	Image          string   `yaml:"image" json:"image"`
	Command        []string `yaml:"command,omitempty" json:"command,omitempty"`
	Next           []string `yaml:"next,omitempty" json:"next,omitempty"`
	DependsOn      []string `yaml:"dependsOn,omitempty" json:"dependsOn,omitempty"`
	container.Spec `yaml:",inline" json:",inline"`
}

// UnmarshalYAML sets defaults while deserialising a step.
func (s *Step) UnmarshalYAML(value *yaml.Node) error {
	type rawStep struct {
		Name           string      `yaml:"name"`
		Engine         string      `yaml:"engine"`
		Image          string      `yaml:"image"`
		Command        []string    `yaml:"command"`
		Next           interface{} `yaml:"next"`
		DependsOn      interface{} `yaml:"dependsOn"`
		container.Spec `yaml:",inline"`
	}

	rs := rawStep{Engine: EngineDocker}
	if err := value.Decode(&rs); err != nil {
		return err
	}

	nextList, err := normalizeInterfaceList(rs.Next)
	if err != nil {
		return fmt.Errorf("step %s next: %w", rs.Name, err)
	}

	dependsList, err := normalizeInterfaceList(rs.DependsOn)
	if err != nil {
		return fmt.Errorf("step %s dependsOn: %w", rs.Name, err)
	}

	s.Name = rs.Name
	s.Engine = rs.Engine
	if s.Engine == "" {
		s.Engine = EngineDocker
	}
	s.Image = rs.Image
	s.Command = rs.Command
	s.Next = nextList
	s.DependsOn = dependsList
	s.Spec = rs.Spec

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
	names, adj, err := computeStepAdjacency(steps)
	if err != nil {
		return err
	}
	if err := detectCycles(adj, names); err != nil {
		return err
	}
	return nil
}

// DeriveStepSuccessors builds the adjacency list for the provided steps.
// It assumes the caller has validated the definition (Validate) beforehand.
func DeriveStepSuccessors(steps []Step) (map[string][]string, error) {
	names, adj, err := computeStepAdjacency(steps)
	if err != nil {
		return nil, err
	}
	result := make(map[string][]string, len(names))
	for name := range names {
		targets := adj[name]
		if len(targets) == 0 {
			continue
		}
		list := make([]string, 0, len(targets))
		for succ := range targets {
			list = append(list, succ)
		}
		sort.Strings(list)
		result[name] = list
	}
	return result, nil
}

func computeStepAdjacency(steps []Step) (map[string]int, map[string]map[string]struct{}, error) {
	names := make(map[string]int, len(steps))
	for i := range steps {
		step := &steps[i]
		if strings.TrimSpace(step.Name) == "" {
			return nil, nil, fmt.Errorf("steps[%d].name is required", i)
		}
		if _, exists := names[step.Name]; exists {
			return nil, nil, fmt.Errorf("duplicate step name %q", step.Name)
		}
		names[step.Name] = i

		if strings.TrimSpace(step.Image) == "" {
			return nil, nil, fmt.Errorf("steps[%d].image is required", i)
		}
		switch step.Engine {
		case EngineDocker, EngineKubernetes, EnginePodman:
		default:
			return nil, nil, fmt.Errorf("steps[%d].engine must be one of [%s,%s,%s]", i, EngineDocker, EngineKubernetes, EnginePodman)
		}

		if err := ensureUnique(step.Next, fmt.Sprintf("steps[%d].next", i)); err != nil {
			return nil, nil, err
		}
		if err := ensureUnique(step.DependsOn, fmt.Sprintf("steps[%d].dependsOn", i)); err != nil {
			return nil, nil, err
		}
	}

	hasExplicitEdges := false

	for i := range steps {
		step := &steps[i]
		steps[i].Next = trimCopy(step.Next)
		steps[i].DependsOn = trimCopy(step.DependsOn)
		if len(steps[i].Next) > 0 || len(steps[i].DependsOn) > 0 {
			hasExplicitEdges = true
		}
	}

	adj := make(map[string]map[string]struct{}, len(steps))

	ensureAdj := func(from string) map[string]struct{} {
		targets, ok := adj[from]
		if !ok {
			targets = make(map[string]struct{})
			adj[from] = targets
		}
		return targets
	}

	for i, step := range steps {
		for _, successor := range step.Next {
			if strings.TrimSpace(successor) == "" {
				return nil, nil, fmt.Errorf("steps[%d].next contains an empty entry", i)
			}
			if _, exists := names[successor]; !exists {
				return nil, nil, fmt.Errorf("steps[%d].next references unknown step %q", i, successor)
			}
			if successor == step.Name {
				return nil, nil, fmt.Errorf("steps[%d].next cannot reference the same step %q", i, successor)
			}
			ensureAdj(step.Name)[successor] = struct{}{}
		}

		for _, dependency := range step.DependsOn {
			if strings.TrimSpace(dependency) == "" {
				return nil, nil, fmt.Errorf("steps[%d].dependsOn contains an empty entry", i)
			}
			if _, exists := names[dependency]; !exists {
				return nil, nil, fmt.Errorf("steps[%d].dependsOn references unknown step %q", i, dependency)
			}
			if dependency == step.Name {
				return nil, nil, fmt.Errorf("steps[%d].dependsOn cannot reference the same step %q", i, dependency)
			}
			ensureAdj(dependency)[step.Name] = struct{}{}
		}
	}

	if !hasExplicitEdges {
		for idx := range steps {
			if len(steps[idx].Next) > 0 {
				continue
			}
			if idx+1 >= len(steps) {
				continue
			}
			ensureAdj(steps[idx].Name)[steps[idx+1].Name] = struct{}{}
		}
	}

	return names, adj, nil
}

func trimCopy(values []string) []string {
	if len(values) == 0 {
		return values
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		out = append(out, v)
	}
	return out
}

func ensureUnique(entries []string, field string) error {
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		key := strings.TrimSpace(entry)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("%s contains duplicate entry %q", field, key)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func normalizeInterfaceList(value interface{}) ([]string, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, nil
		}
		return []string{strings.TrimSpace(v)}, nil
	case []interface{}:
		result := make([]string, 0, len(v))
		for idx, item := range v {
			str, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("entry %d must be a string", idx)
			}
			str = strings.TrimSpace(str)
			if str == "" {
				return nil, fmt.Errorf("entry %d cannot be empty", idx)
			}
			result = append(result, str)
		}
		return result, nil
	case []string:
		result := make([]string, 0, len(v))
		for idx, item := range v {
			item = strings.TrimSpace(item)
			if item == "" {
				return nil, fmt.Errorf("entry %d cannot be empty", idx)
			}
			result = append(result, item)
		}
		return result, nil
	default:
		return nil, fmt.Errorf("expected string or list, got %T", value)
	}
}

func detectCycles(adj map[string]map[string]struct{}, names map[string]int) error {
	const (
		unvisited = iota
		visiting
		visited
	)

	state := make(map[string]int, len(names))

	var visit func(string) error
	visit = func(node string) error {
		switch state[node] {
		case visiting:
			return fmt.Errorf("cycle detected involving step %q", node)
		case visited:
			return nil
		}

		state[node] = visiting
		for succ := range adj[node] {
			if err := visit(succ); err != nil {
				return err
			}
		}
		state[node] = visited
		return nil
	}

	for name := range names {
		if err := visit(name); err != nil {
			return err
		}
	}
	return nil
}
