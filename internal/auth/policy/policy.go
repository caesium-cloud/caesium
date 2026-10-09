// Package policy parses and resolves namespace access policies without runtime
// or authentication-provider dependencies.
package policy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/caesium-cloud/caesium/internal/models"
	"gopkg.in/yaml.v3"
)

const (
	APIVersionV1     = "v1"
	KindAccessPolicy = "AccessPolicy"
	DefaultNamespace = "default"
	ClusterNamespace = "*"
)

var (
	namespaceLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	dnsSubdomain   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
)

// AccessPolicy declares namespaces and the identity bindings that grant access.
type AccessPolicy struct {
	APIVersion string               `yaml:"apiVersion" json:"apiVersion"`
	Kind       string               `yaml:"kind" json:"kind"`
	Namespaces map[string]Namespace `yaml:"namespaces,omitempty" json:"namespaces"`
	Bindings   []Binding            `yaml:"bindings,omitempty" json:"bindings"`
}

// Namespace contains the isolation settings for one tenancy namespace.
type Namespace struct {
	Kubernetes *KubernetesSettings `yaml:"kubernetes,omitempty" json:"kubernetes,omitempty"`
	Secrets    *SecretRules        `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	Quotas     *Quotas             `yaml:"quotas,omitempty" json:"quotas,omitempty"`
}

// KubernetesSettings overrides the cluster's default execution target.
// Empty fields retain the cluster defaults.
type KubernetesSettings struct {
	Namespace          string `yaml:"namespace,omitempty" json:"namespace,omitempty"`
	ServiceAccountName string `yaml:"serviceAccountName,omitempty" json:"serviceAccountName,omitempty"`
}

// SecretRules is an allow-list of canonical provider/path globs. A nil Secrets
// pointer means allow all; a present SecretRules with no entries means deny all.
type SecretRules struct {
	Allow []string `yaml:"allow" json:"allow"`
}

// Quotas limits concurrent runs; zero means unlimited.
type Quotas struct {
	MaxConcurrentRuns int `yaml:"maxConcurrentRuns,omitempty" json:"maxConcurrentRuns,omitempty"`
}

// Binding grants a role in each listed namespace when any subject matches.
type Binding struct {
	Subjects   Subjects    `yaml:"subjects" json:"subjects"`
	Role       models.Role `yaml:"role" json:"role"`
	Namespaces []string    `yaml:"namespaces" json:"namespaces"`
}

// Subjects matches exact IdP groups (or "*" for all authenticated users) and
// ASCII case-insensitive user emails. Other group strings are never patterns.
type Subjects struct {
	Groups []string `yaml:"groups,omitempty" json:"groups,omitempty"`
	Users  []string `yaml:"users,omitempty" json:"users,omitempty"`
}

// Parse accepts one strictly typed YAML document, rejects unknown fields and
// duplicate keys, validates its settings, and adds the implicit default namespace.
func Parse(data []byte) (*AccessPolicy, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := dec.Decode(&document); err != nil {
		return nil, fmt.Errorf("parse access policy: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, fmt.Errorf("parse access policy: %w", err)
		}
		return nil, errors.New("access policy must contain a single YAML document")
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("access policy must be a YAML mapping")
	}
	walker := yamlValidator{active: make(map[*yaml.Node]bool), validated: make(map[yamlValidationKey]bool)}
	if err := walker.validate(document.Content[0], reflect.TypeFor[AccessPolicy](), "policy"); err != nil {
		return nil, err
	}
	var p AccessPolicy
	if err := document.Content[0].Decode(&p); err != nil {
		return nil, fmt.Errorf("parse access policy: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if p.Namespaces == nil {
		p.Namespaces = make(map[string]Namespace)
	}
	if _, exists := p.Namespaces[DefaultNamespace]; !exists {
		p.Namespaces[DefaultNamespace] = Namespace{}
	}
	for _, namespace := range p.Namespaces {
		if namespace.Secrets != nil && namespace.Secrets.Allow == nil {
			namespace.Secrets.Allow = []string{}
		}
	}
	for i := range p.Bindings {
		for j, email := range p.Bindings[i].Subjects.Users {
			p.Bindings[i].Subjects.Users[j] = foldASCII(email)
		}
	}
	return &p, nil
}

// Validate checks policy semantics without changing the policy. The default
// namespace is declared even when it is absent from Namespaces.
func (p *AccessPolicy) Validate() error {
	if p == nil {
		return errors.New("access policy is required")
	}
	if p.APIVersion != APIVersionV1 {
		return fmt.Errorf("unsupported access policy apiVersion %q (want %q)", p.APIVersion, APIVersionV1)
	}
	if p.Kind != KindAccessPolicy {
		return fmt.Errorf("unsupported access policy kind %q (want %q)", p.Kind, KindAccessPolicy)
	}
	for _, name := range namespaceNames(p.Namespaces) {
		if !namespaceLabel.MatchString(name) {
			if name == ClusterNamespace {
				return fmt.Errorf("namespaces[%q]: namespace must be a DNS label; %q is reserved for cluster grants", name, ClusterNamespace)
			}
			return fmt.Errorf("namespaces[%q]: namespace must be a DNS label", name)
		}
		ns := p.Namespaces[name]
		if ns.Kubernetes != nil {
			if target := ns.Kubernetes.Namespace; target != "" && !namespaceLabel.MatchString(target) {
				return fmt.Errorf("namespaces[%q].kubernetes.namespace: must be a DNS label", name)
			}
			if account := ns.Kubernetes.ServiceAccountName; account != "" && !validDNSSubdomain(account) {
				return fmt.Errorf("namespaces[%q].kubernetes.serviceAccountName: must be a DNS subdomain", name)
			}
		}
		if ns.Quotas != nil && ns.Quotas.MaxConcurrentRuns < 0 {
			return fmt.Errorf("namespaces[%q].quotas.maxConcurrentRuns: must not be negative", name)
		}
		if ns.Secrets != nil {
			for i, glob := range ns.Secrets.Allow {
				if err := ValidateGlob(glob); err != nil {
					return fmt.Errorf("namespaces[%q].secrets.allow[%d]: %w", name, i, err)
				}
			}
		}
	}
	for i, binding := range p.Bindings {
		if !models.ValidRole(string(binding.Role)) {
			return fmt.Errorf("bindings[%d].role: invalid role %q", i, binding.Role)
		}
		if len(binding.Subjects.Groups)+len(binding.Subjects.Users) == 0 {
			return fmt.Errorf("bindings[%d].subjects: at least one group or user is required", i)
		}
		for _, subjects := range []struct {
			name   string
			values []string
		}{{"groups", binding.Subjects.Groups}, {"users", binding.Subjects.Users}} {
			for j, subject := range subjects.values {
				if strings.TrimSpace(subject) == "" {
					return fmt.Errorf("bindings[%d].subjects.%s[%d]: must not be empty", i, subjects.name, j)
				}
			}
		}
		if len(binding.Namespaces) == 0 {
			return fmt.Errorf("bindings[%d].namespaces: at least one namespace is required", i)
		}
		for _, name := range binding.Namespaces {
			if name == ClusterNamespace || name == DefaultNamespace {
				continue
			}
			if _, declared := p.Namespaces[name]; !declared {
				return fmt.Errorf("bindings[%d].namespaces: undeclared namespace %q", i, name)
			}
		}
	}
	return nil
}

// Warnings returns deterministic offline lint warnings. Known-user group checks
// require runtime identity data and belong to the server-side lint consumer.
func (p *AccessPolicy) Warnings() []string {
	var warnings []string
	if p == nil {
		return warnings
	}
	for _, name := range namespaceNames(p.Namespaces) {
		if name != DefaultNamespace && p.Namespaces[name].Secrets == nil {
			warnings = append(warnings, fmt.Sprintf("namespace %q has no secret rules; all secrets are allowed", name))
		}
	}
	return warnings
}

func namespaceNames(namespaces map[string]Namespace) []string {
	names := make([]string, 0, len(namespaces))
	for name := range namespaces {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func validDNSSubdomain(name string) bool {
	// Kubernetes service accounts use DNS1123 subdomain validation: the whole
	// name is limited to 253 characters, without a per-label 63-character cap.
	return len(name) <= 253 && dnsSubdomain.MatchString(name)
}

type yamlValidationKey struct {
	node *yaml.Node
	typ  reflect.Type
}

type yamlValidator struct {
	active    map[*yaml.Node]bool
	validated map[yamlValidationKey]bool
}

// yaml.v3 normally coerces numeric and boolean scalars into strings and treats
// null settings as omitted. Reject those ambiguities before typed decoding,
// especially null secret rules, which must never become an allow-all default.
func (v *yamlValidator) validate(node *yaml.Node, typ reflect.Type, path string) (err error) {
	if node == nil {
		return fmt.Errorf("%s: missing YAML node", path)
	}
	if v.active[node] {
		return fmt.Errorf("%s: cyclic YAML alias", path)
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	key := yamlValidationKey{node: node, typ: typ}
	if v.validated[key] {
		return nil
	}
	v.active[node] = true
	defer func() {
		delete(v.active, node)
		if err == nil {
			v.validated[key] = true
		}
	}()
	if node.Kind == yaml.AliasNode {
		return v.validate(node.Alias, typ, path)
	}
	errType := func(want string) error {
		return fmt.Errorf("%s (line %d): expected %s", path, node.Line, want)
	}
	switch typ.Kind() {
	case reflect.Struct, reflect.Map:
		if node.Kind != yaml.MappingNode {
			return errType("a mapping")
		}
		seen := make(map[string]bool)
		for i := 0; i < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Tag != "!!str" {
				return fmt.Errorf("%s (line %d): mapping keys must be strings; YAML merge keys are unsupported", path, key.Line)
			}
			if seen[key.Value] {
				return fmt.Errorf("%s (line %d): duplicate key %q", path, key.Line, key.Value)
			}
			seen[key.Value] = true
			var childType reflect.Type
			if typ.Kind() == reflect.Map {
				childType = typ.Elem()
			} else {
				for j := 0; j < typ.NumField(); j++ {
					field := typ.Field(j)
					name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
					if key.Value == name {
						childType = field.Type
						break
					}
				}
				if childType == nil {
					return fmt.Errorf("%s (line %d): unknown field %q", path, key.Line, key.Value)
				}
			}
			childPath := path + "." + key.Value
			if typ.Kind() == reflect.Map {
				childPath = fmt.Sprintf("%s[%q]", path, key.Value)
			}
			if err := v.validate(value, childType, childPath); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if node.Kind != yaml.SequenceNode {
			return errType("a sequence")
		}
		for i, child := range node.Content {
			if err := v.validate(child, typ.Elem(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case reflect.String:
		if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
			return errType("a string")
		}
	case reflect.Int:
		if node.Kind != yaml.ScalarNode || node.Tag != "!!int" {
			return errType("an integer")
		}
	default:
		return fmt.Errorf("%s (line %d): unsupported policy field type %s", path, node.Line, typ)
	}
	return nil
}
