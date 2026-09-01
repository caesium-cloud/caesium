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

// cacheKeyPattern is a single, lower-case path element: alphanumeric, then up
// to 62 of alphanumeric / '.' / '_' / '-'. Lower-case is intentional. Docker
// bind mounts can inherit a case-insensitive host filesystem, where allowing
// both "deploy" and "Deploy" would alias two supposedly independent slots.
// Rejecting case variants keeps the same cache layout safe on named volumes,
// PVCs and bind mounts.
var cacheKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

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
	// Whitespace-only stays the unkeyed compatibility case, but a non-empty key
	// is never normalized. Silently turning " deploy " into "deploy" would make
	// two manifest values address one file despite the reject-don't-rewrite
	// contract this function exists to enforce.
	if key != raw {
		return "", fmt.Errorf("CACHE_KEY %q must not contain leading or trailing whitespace", raw)
	}
	if strings.ContainsRune(key, '/') || strings.ContainsRune(key, '\\') ||
		strings.ContainsRune(key, filepath.Separator) {
		return "", fmt.Errorf("CACHE_KEY %q must be a single path element", key)
	}
	if key == "." || key == ".." || strings.HasPrefix(key, ".") {
		return "", fmt.Errorf("CACHE_KEY %q must not start with '.' (reserved for volume bookkeeping)", key)
	}
	folded := strings.ToLower(key)
	if strings.HasPrefix(folded, "providers.tmp.") {
		return "", fmt.Errorf("CACHE_KEY %q collides with tf-warm staging directories", key)
	}
	if _, reserved := reservedCacheKeys[folded]; reserved {
		return "", fmt.Errorf("CACHE_KEY %q is reserved by the cache volume layout", key)
	}
	if !cacheKeyPattern.MatchString(key) {
		return "", fmt.Errorf("CACHE_KEY %q is not a valid slot name (use lower-case letters, digits, '.', '_' and '-', starting with alphanumeric, at most 63 characters)", key)
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
