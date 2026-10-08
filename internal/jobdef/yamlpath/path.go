package yamlpath

import (
	"path/filepath"
	"strings"
)

// IsYAML reports whether path has a YAML extension, ignoring extension case.
func IsYAML(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".yaml" || ext == ".yml"
}
