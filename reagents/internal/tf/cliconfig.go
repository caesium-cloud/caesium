package tf

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// DefaultCacheDir is the cache volume mount both tf-warm and tf-runner use
// when CACHE_DIR is unset.
const DefaultCacheDir = "/cache"

// TerraformRCName is the CLI configuration file tf-warm writes and every
// consuming step points TF_CLI_CONFIG_FILE at.
const TerraformRCName = "terraformrc"

// cacheKeyPattern is a single path element: alphanumeric, then up to 62 of
// alphanumeric / '.' / '_' / '-'. It is the shape that can sit next to the
// volume's own `providers/` and `.warm/` directories without becoming a
// traversal or a collision.
var cacheKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// reservedCacheKeys are names the cache volume already uses. A CACHE_KEY of
// `providers` would write terraformrc into the mirror root; `terraformrc`
// would turn the unkeyed config file into a directory.
var reservedCacheKeys = map[string]struct{}{
	"providers":     {},
	TerraformRCName: {},
}

// SanitizeCacheKey returns a filesystem-safe slot name for CACHE_KEY.
//
// An empty (or whitespace-only) value is the unkeyed default slot, so existing
// manifests that never set CACHE_KEY keep writing and reading
// `{cacheDir}/terraformrc`. Anything else is rejected rather than rewritten:
// folding `foo/bar` into `foo-bar` would let two jobs collide on one file and
// look configured.
func SanitizeCacheKey(raw string) (string, error) {
	key := strings.TrimSpace(raw)
	if key == "" {
		return "", nil
	}
	if strings.ContainsRune(key, '/') || strings.ContainsRune(key, '\\') ||
		strings.ContainsRune(key, filepath.Separator) {
		return "", fmt.Errorf("CACHE_KEY %q must be a single path element", key)
	}
	if key == "." || key == ".." || strings.HasPrefix(key, ".") {
		return "", fmt.Errorf("CACHE_KEY %q must not start with '.' (reserved for volume bookkeeping)", key)
	}
	if strings.HasPrefix(key, "providers.tmp.") {
		return "", fmt.Errorf("CACHE_KEY %q collides with tf-warm staging directories", key)
	}
	if _, reserved := reservedCacheKeys[key]; reserved {
		return "", fmt.Errorf("CACHE_KEY %q is reserved by the cache volume layout", key)
	}
	if !cacheKeyPattern.MatchString(key) {
		return "", fmt.Errorf("CACHE_KEY %q is not a valid slot name (use letters, digits, '.', '_' and '-', starting with alphanumeric, at most 63 characters)", key)
	}
	return key, nil
}

// CLIConfigFile is the terraformrc path for a cache volume slot.
//
//	CACHE_KEY unset  → {cacheDir}/terraformrc
//	CACHE_KEY set    → {cacheDir}/{key}/terraformrc
//
// cacheKey must already have passed SanitizeCacheKey. The path is the one the
// process whose CacheDir this is will open — warm writes it; the runner
// exports it as TF_CLI_CONFIG_FILE.
func CLIConfigFile(cacheDir, cacheKey string) string {
	if cacheKey == "" {
		return filepath.Join(cacheDir, TerraformRCName)
	}
	return filepath.Join(cacheDir, cacheKey, TerraformRCName)
}
