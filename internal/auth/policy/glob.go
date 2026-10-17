package policy

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

var providerName = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// secretProviders are the secret:// providers a rule may name, mapped to their
// canonical spelling: kubernetes is an alias of k8s, exactly as the resolver
// chain registers it (internal/jobdef/secret NewConfiguredResolver). A parity
// test in that package pins this set to the registered providers.
var secretProviders = map[string]string{
	"env":        "env",
	"k8s":        "k8s",
	"kubernetes": "k8s",
	"vault":      "vault",
}

// SecretProviders returns the sorted provider names a secret rule may use,
// aliases included.
func SecretProviders() []string {
	names := make([]string, 0, len(secretProviders))
	for name := range secretProviders {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// CanonicalSecretProvider maps a supported provider name to its canonical
// spelling (kubernetes -> k8s). It returns "" for an unsupported provider.
func CanonicalSecretProvider(provider string) string {
	return secretProviders[provider]
}

// ValidateGlob checks a provider/path rule. Providers must be supported
// secret:// providers (env, k8s or its alias kubernetes, vault). Each path
// segment follows path.Match syntax; a whole unescaped "**" segment matches zero
// or more segments. In other segments, repeated stars, escapes and character
// classes retain path.Match semantics within that segment. Neither patterns nor
// targets are cleaned. Vault rules select paths, covering all fields; a target's
// nonempty #field suffix is ignored. # is reserved as that delimiter, and targets
// with multiple delimiters fail closed. Field patterns are unsupported.
func ValidateGlob(pattern string) error {
	provider, rest, ok := strings.Cut(pattern, "/")
	if !ok || !providerName.MatchString(provider) || rest == "" {
		return fmt.Errorf("invalid secret glob %q: expected provider/path", pattern)
	}
	if CanonicalSecretProvider(provider) == "" {
		return fmt.Errorf("invalid secret glob %q: unsupported provider %q (want one of %s)", pattern, provider, strings.Join(SecretProviders(), ", "))
	}
	if provider == "vault" && strings.Contains(rest, "#") {
		return fmt.Errorf("invalid secret glob %q: Vault rules select paths without #field", pattern)
	}
	for segment := range strings.SplitSeq(rest, "/") {
		if segment == "" {
			return fmt.Errorf("invalid secret glob %q: empty path segment", pattern)
		}
		if segment == "." || segment == ".." {
			return fmt.Errorf("invalid secret glob %q: dot path segments are unsupported", pattern)
		}
		if segment == "**" {
			continue
		}
		if _, err := path.Match(segment, ""); err != nil {
			return fmt.Errorf("invalid secret glob %q: %w", pattern, err)
		}
	}
	return nil
}

// MatchGlob matches a validated provider/path rule against a canonical target.
// A single star never crosses a slash. Globstar can cross any number of segments,
// including zero; the provider itself can never be matched by a wildcard.
// Providers compare by their canonical spelling, so a kubernetes/ rule matches
// a k8s/ target.
func MatchGlob(pattern, target string) (bool, error) {
	if err := ValidateGlob(pattern); err != nil {
		return false, err
	}
	provider, rest, ok := strings.Cut(target, "/")
	if !ok || !providerName.MatchString(provider) || rest == "" {
		return false, nil
	}
	provider = CanonicalSecretProvider(provider)
	if provider == "" {
		return false, nil
	}
	if provider == "vault" {
		if strings.Count(rest, "#") > 1 {
			return false, nil
		}
		var field string
		var hasField bool
		rest, field, hasField = strings.Cut(rest, "#")
		if hasField && field == "" {
			return false, nil
		}
	}
	parts := strings.Split(rest, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false, nil
		}
	}
	patternProvider, patternPath, _ := strings.Cut(pattern, "/")
	if provider != CanonicalSecretProvider(patternProvider) {
		return false, nil
	}
	segments := strings.Split(patternPath, "/")
	// Dynamic programming bounds repeated globstars to O(pattern * target),
	// rather than recursively exploring every split of an untrusted target.
	previous := make([]bool, len(parts)+1)
	previous[0] = true
	for _, segment := range segments {
		current := make([]bool, len(parts)+1)
		if segment == "**" {
			current[0] = previous[0]
			for j := 1; j <= len(parts); j++ {
				current[j] = previous[j] || current[j-1]
			}
		} else {
			for j := 1; j <= len(parts); j++ {
				if !previous[j-1] {
					continue
				}
				matched, _ := path.Match(segment, parts[j-1]) // syntax checked above
				current[j] = matched
			}
		}
		previous = current
	}
	return previous[len(parts)], nil
}
