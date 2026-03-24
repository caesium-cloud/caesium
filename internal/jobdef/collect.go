// Package jobdef provides job definition utilities including collection
// and import of YAML manifests.
package jobdef

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"gopkg.in/yaml.v3"
)

// CollectDefinitions walks the given paths, reads YAML files, and returns
// all valid Caesium job definitions found. Non-Caesium YAML documents
// (e.g. Helm charts, Kubernetes manifests) are silently skipped.
// If validate is true, each definition is validated and errors are returned.
func CollectDefinitions(paths []string, validate bool) ([]schema.Definition, error) {
	if len(paths) == 0 {
		paths = []string{"."}
	}

	var defs []schema.Definition
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
				if d.IsDir() || !IsYAML(path) {
					return nil
				}
				return appendDefinitions(path, &defs, validate)
			}); err != nil {
				return nil, err
			}
		} else {
			if !IsYAML(p) {
				return nil, fmt.Errorf("%s is not a YAML file", p)
			}
			if err := appendDefinitions(p, &defs, validate); err != nil {
				return nil, err
			}
		}
	}
	return defs, nil
}

func appendDefinitions(path string, defs *[]schema.Definition, validate bool) error {
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
			// Skip files that fail to decode as a Definition — they are
			// likely non-Caesium YAML (Helm charts, K8s manifests, etc.).
			return nil
		}
		if isBlankDefinition(&def) {
			continue
		}
		if !isCaesiumDefinition(&def) {
			continue
		}
		if validate {
			if err := def.Validate(); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
		}
		*defs = append(*defs, def)
	}

	return nil
}

// isCaesiumDefinition returns true if the definition looks like a Caesium
// job (has kind=Job and a recognised apiVersion). This allows silently
// skipping non-Caesium YAML that happens to live in the same directory tree.
func isCaesiumDefinition(def *schema.Definition) bool {
	return def.Kind == schema.KindJob && def.APIVersion == schema.APIVersionV1
}

func isBlankDefinition(def *schema.Definition) bool {
	if def == nil {
		return true
	}
	if strings.TrimSpace(def.Metadata.Alias) != "" {
		return false
	}
	if def.APIVersion != "" || def.Kind != "" {
		return false
	}
	if def.Trigger.Type != "" || len(def.Steps) > 0 || len(def.Callbacks) > 0 {
		return false
	}
	return true
}

// IsYAML returns true if the file path has a .yaml or .yml extension.
func IsYAML(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".yaml" || ext == ".yml"
}

// ResolveYAMLFiles returns all YAML file paths under the given paths.
func ResolveYAMLFiles(paths []string) ([]string, error) {
	if len(paths) == 0 {
		paths = []string{"."}
	}

	var files []string
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
				if !d.IsDir() && IsYAML(path) {
					files = append(files, path)
				}
				return nil
			}); err != nil {
				return nil, err
			}
		} else if IsYAML(p) {
			files = append(files, p)
		}
	}
	return files, nil
}
