// Package yamlstrict validates YAML mapping keys against Go struct tags before
// yaml.v3 decodes them. It complements typed decoding by rejecting misspelled
// structural fields while leaving intentionally free-form maps open.
package yamlstrict

import (
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// ValidateKnownFields rejects mapping keys that are not represented by the
// YAML tags on target. Errors include the YAML path and source line.
func ValidateKnownFields(node *yaml.Node, target any) error {
	if node == nil || target == nil {
		return nil
	}
	var findings []string
	validate(node, reflect.TypeOf(target), "", make(map[visit]bool), &findings)
	if len(findings) == 0 {
		return nil
	}
	return fmt.Errorf("unknown YAML fields:\n%s", strings.Join(findings, "\n"))
}

type visit struct {
	node *yaml.Node
	typ  reflect.Type
}

func validate(node *yaml.Node, typ reflect.Type, path string, active map[visit]bool, findings *[]string) {
	node = contentNode(node)
	typ = indirectType(typ)
	if node == nil || typ == nil || typ.Kind() == reflect.Interface {
		return
	}

	v := visit{node: node, typ: typ}
	if active[v] {
		return
	}
	active[v] = true
	defer delete(active, v)

	switch typ.Kind() {
	case reflect.Struct:
		if node.Kind != yaml.MappingNode {
			return
		}
		fields := structFields(typ, make(map[reflect.Type]bool))
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if isMergeKey(key) {
				validateMerge(value, typ, path, active, findings)
				continue
			}
			field, ok := fields[key.Value]
			fieldPath := joinPath(path, key.Value)
			if !ok {
				*findings = append(*findings, fmt.Sprintf("  YAML path %s (line %d): unknown field %q", fieldPath, key.Line, key.Value))
				continue
			}
			if len(field.allowedKeys) > 0 {
				validateAllowedMapping(value, field.allowedKeys, fieldPath, make(map[*yaml.Node]bool), findings)
				continue
			}
			validate(value, field.typ, fieldPath, active, findings)
		}
	case reflect.Slice, reflect.Array:
		if node.Kind != yaml.SequenceNode {
			return
		}
		for i, child := range node.Content {
			validate(child, typ.Elem(), fmt.Sprintf("%s[%d]", path, i), active, findings)
		}
	case reflect.Map:
		elemType := indirectType(typ.Elem())
		if node.Kind != yaml.MappingNode || elemType == nil || elemType.Kind() == reflect.Interface {
			return
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if isMergeKey(key) {
				validateMerge(value, typ, path, active, findings)
				continue
			}
			validate(value, typ.Elem(), joinPath(path, key.Value), active, findings)
		}
	}
}

func validateMerge(node *yaml.Node, typ reflect.Type, path string, active map[visit]bool, findings *[]string) {
	node = contentNode(node)
	if node == nil {
		return
	}
	if node.Kind == yaml.SequenceNode {
		for _, child := range node.Content {
			validate(child, typ, path, active, findings)
		}
		return
	}
	validate(node, typ, path, active, findings)
}

func contentNode(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return nil
		}
		node = node.Content[0]
	}
	seen := make(map[*yaml.Node]bool)
	for node != nil && node.Kind == yaml.AliasNode {
		if seen[node] {
			return nil
		}
		seen[node] = true
		node = node.Alias
	}
	return node
}

func indirectType(typ reflect.Type) reflect.Type {
	for typ != nil && typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	return typ
}

type fieldSpec struct {
	typ         reflect.Type
	allowedKeys map[string]struct{}
}

func structFields(typ reflect.Type, active map[reflect.Type]bool) map[string]fieldSpec {
	typ = indirectType(typ)
	fields := make(map[string]fieldSpec)
	if typ == nil || typ.Kind() != reflect.Struct || active[typ] {
		return fields
	}
	active[typ] = true
	defer delete(active, typ)

	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.PkgPath != "" {
			continue
		}
		name, options, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if name == "-" {
			continue
		}
		if hasOption(options, "inline") {
			for inlineName, inlineField := range structFields(field.Type, active) {
				fields[inlineName] = inlineField
			}
			continue
		}
		if name == "" {
			name = strings.ToLower(field.Name)
		}
		fields[name] = fieldSpec{
			typ:         field.Type,
			allowedKeys: parseAllowedKeys(field.Tag.Get("yamlstrict")),
		}
	}
	return fields
}

func parseAllowedKeys(tag string) map[string]struct{} {
	if tag == "" {
		return nil
	}
	keys := make(map[string]struct{})
	for _, key := range strings.Split(tag, ",") {
		if key = strings.TrimSpace(key); key != "" {
			keys[key] = struct{}{}
		}
	}
	return keys
}

func validateAllowedMapping(node *yaml.Node, allowed map[string]struct{}, path string, active map[*yaml.Node]bool, findings *[]string) {
	node = contentNode(node)
	if node == nil || node.Kind != yaml.MappingNode {
		return
	}
	if active[node] {
		return
	}
	active[node] = true
	defer delete(active, node)

	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i]
		if isMergeKey(key) {
			validateAllowedMerge(node.Content[i+1], allowed, path, active, findings)
			continue
		}
		if _, ok := allowed[key.Value]; !ok {
			fieldPath := joinPath(path, key.Value)
			*findings = append(*findings, fmt.Sprintf("  YAML path %s (line %d): unknown field %q", fieldPath, key.Line, key.Value))
		}
	}
}

func validateAllowedMerge(node *yaml.Node, allowed map[string]struct{}, path string, active map[*yaml.Node]bool, findings *[]string) {
	node = contentNode(node)
	if node == nil {
		return
	}
	if node.Kind == yaml.SequenceNode {
		for _, child := range node.Content {
			validateAllowedMapping(child, allowed, path, active, findings)
		}
		return
	}
	validateAllowedMapping(node, allowed, path, active, findings)
}

func hasOption(options, want string) bool {
	for _, option := range strings.Split(options, ",") {
		if option == want {
			return true
		}
	}
	return false
}

func isMergeKey(node *yaml.Node) bool {
	return node != nil && node.Tag == "!!merge"
}

func joinPath(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}
