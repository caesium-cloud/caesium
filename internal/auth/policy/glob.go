package policy

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

var providerName = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// ValidateGlob checks a provider/path rule. Providers are exact names. Each path
// segment follows path.Match syntax; a whole unescaped "**" segment matches zero
// or more segments. In other segments, repeated stars, escapes and character
// classes retain path.Match semantics within that segment. Neither patterns nor
// targets are cleaned or otherwise normalised.
func ValidateGlob(pattern string) error {
	provider, rest, ok := strings.Cut(pattern, "/")
	if !ok || !providerName.MatchString(provider) || rest == "" {
		return fmt.Errorf("invalid secret glob %q: expected provider/path", pattern)
	}
	for segment := range strings.SplitSeq(rest, "/") {
		if segment == "" {
			return fmt.Errorf("invalid secret glob %q: empty path segment", pattern)
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
func MatchGlob(pattern, target string) (bool, error) {
	if err := ValidateGlob(pattern); err != nil {
		return false, err
	}
	provider, rest, ok := strings.Cut(target, "/")
	if !ok || !providerName.MatchString(provider) || rest == "" {
		return false, nil
	}
	parts := strings.Split(rest, "/")
	for _, part := range parts {
		if part == "" {
			return false, nil
		}
	}
	patternProvider, patternPath, _ := strings.Cut(pattern, "/")
	if provider != patternProvider {
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
				matched, _ := path.Match(segment, parts[j-1]) // syntax checked above
				current[j] = previous[j-1] && matched
			}
		}
		previous = current
	}
	return previous[len(parts)], nil
}
