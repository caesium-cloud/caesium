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
		var document yaml.Node
		if err := dec.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("%s: %w", path, err)
		}
		if isBlankDocument(&document) {
			continue
		}
		isJob, err := isCaesiumDocument(path, &document)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if !isJob {
			continue
		}

		var def schema.Definition
		if err := document.Decode(&def); err != nil {
			return fmt.Errorf("%s: %w", path, err)
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

// isCaesiumDocument identifies a Caesium job from its raw header before typed
// decoding. Kubernetes Jobs use grouped API versions such as batch/v1 and are
// intentionally left alone; ungrouped Job versions are Caesium manifests so an
// unsupported or missing version still reaches validation instead of vanishing.
func isCaesiumDocument(path string, document *yaml.Node) (bool, error) {
	root := documentRoot(document)
	if root == nil || root.Kind != yaml.MappingNode {
		return isJobManifestPath(path), nil
	}
	var header map[string]any
	if err := document.Decode(&header); err != nil {
		return false, err
	}
	if isJobManifestPath(path) {
		return true, nil
	}
	kind, kindOK := header["kind"].(string)
	if !kindOK || strings.TrimSpace(kind) != schema.KindJob {
		return false, nil
	}
	apiVersion, apiVersionOK := header["apiVersion"].(string)
	return !apiVersionOK || !strings.Contains(strings.TrimSpace(apiVersion), "/"), nil
}

func isBlankDocument(document *yaml.Node) bool {
	root := documentRoot(document)
	return root == nil || (root.Kind == yaml.ScalarNode && root.Tag == "!!null")
}

func documentRoot(document *yaml.Node) *yaml.Node {
	if document == nil {
		return nil
	}
	if document.Kind == yaml.DocumentNode {
		if len(document.Content) == 0 {
			return nil
		}
		return document.Content[0]
	}
	return document
}

func isJobManifestPath(path string) bool {
	lower := strings.ToLower(path)
	return strings.HasSuffix(lower, ".job.yaml") || strings.HasSuffix(lower, ".job.yml")
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
