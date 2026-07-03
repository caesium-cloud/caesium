package incident

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// hexTokenRe matches a purely hexadecimal token (git commit SHAs, Docker image
// digests, hashes). These have a high character-class mix and high entropy, so
// the entropy heuristic would flag them — but they are non-sensitive identifiers
// whose redaction only destroys log readability. Excluded from token scrubbing.
var hexTokenRe = regexp.MustCompile(`^[0-9a-fA-F]+$`)

// Redacted is the placeholder substituted for a scrubbed secret value or token.
const Redacted = "[REDACTED]"

// minScrubLength is the minimum length a resolved secret value must have before
// the scrubber will blanket-replace it by exact match. Below this, an exact
// replacement of a common short value (e.g. "true", "8080") would destroy
// unrelated log text, so it is skipped — we rely on the structured-env knowledge
// that the value came from a secret:// ref rather than blind string matching.
const minScrubLength = 6

// literalDenylist is a small set of common boolean/word literals that must never
// be blanket-replaced even if a secret resolves to them (structured-env
// over-redaction guard). Small integers are handled separately by the
// numeric check.
var literalDenylist = map[string]struct{}{
	"true":     {},
	"false":    {},
	"null":     {},
	"none":     {},
	"nil":      {},
	"password": {},
	"secret":   {},
	"token":    {},
	"changeme": {},
	"admin":    {},
}

// highEntropyTokenRe matches token-shaped substrings (long runs of
// base64/hex/url-safe characters) that are candidates for entropy-based
// redaction. The min length is deliberately high to avoid catching ordinary
// words, paths, or identifiers.
var highEntropyTokenRe = regexp.MustCompile(`[A-Za-z0-9+/=_\-]{20,}`)

// Scrubber removes secret material from free-text log output before it enters a
// triage bundle, an agent-readable endpoint, or an escalation message. It does
// exact removal of every resolved secret:// value (subject to the
// over-redaction guard) plus a conservative high-entropy token heuristic.
type Scrubber struct {
	// secrets is the deduplicated, guard-filtered set of exact values to remove,
	// sorted longest-first so a longer secret that contains a shorter one is
	// replaced before the shorter match can fragment it.
	secrets []string
}

// NewScrubber builds a Scrubber from a set of resolved secret values. Values
// that fail the over-redaction guard (too short, a denylisted literal, or a
// bare small number) are dropped from exact-match scrubbing.
func NewScrubber(secretValues []string) *Scrubber {
	seen := make(map[string]struct{}, len(secretValues))
	kept := make([]string, 0, len(secretValues))
	for _, v := range secretValues {
		if !scrubbable(v) {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		kept = append(kept, v)
	}
	sort.SliceStable(kept, func(i, j int) bool {
		return len(kept[i]) > len(kept[j])
	})
	return &Scrubber{secrets: kept}
}

// SecretValuesFromEnv extracts the resolved values of the env keys whose raw
// value was a secret:// reference. raw is the pre-resolution env (key -> raw
// value, some carrying the secret:// scheme); resolved is the post-resolution
// env (key -> actual value). Only keys that were secret refs contribute, so a
// literal env value that merely looks sensitive is not scrubbed here.
func SecretValuesFromEnv(raw, resolved map[string]string) []string {
	const secretRefPrefix = "secret://"
	out := make([]string, 0)
	for k, rawVal := range raw {
		if !strings.HasPrefix(rawVal, secretRefPrefix) {
			continue
		}
		if val, ok := resolved[k]; ok && val != "" {
			out = append(out, val)
		}
	}
	return out
}

// scrubbable reports whether a resolved secret value is safe to blanket-replace
// by exact match without destroying unrelated log text.
func scrubbable(v string) bool {
	if len(v) < minScrubLength {
		return false
	}
	if _, denied := literalDenylist[strings.ToLower(v)]; denied {
		return false
	}
	if isAllDigits(v) {
		// A bare number (even a long one) is too likely to collide with
		// unrelated log content (timestamps, ids); skip exact scrubbing.
		return false
	}
	return true
}

func isAllDigits(v string) bool {
	if v == "" {
		return false
	}
	for _, r := range v {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Scrub returns text with every guard-passing secret value removed by exact
// replacement and every high-entropy token redacted. The exact-match pass runs
// first so a known secret is always removed even if the entropy heuristic would
// have missed it.
func (s *Scrubber) Scrub(text string) string {
	if text == "" {
		return text
	}
	out := text
	for _, secret := range s.secrets {
		out = strings.ReplaceAll(out, secret, Redacted)
	}
	out = s.scrubHighEntropy(out)
	return out
}

// scrubHighEntropy replaces token-shaped substrings whose Shannon entropy is
// high enough to look like a credential. The bar is intentionally conservative
// (long, mixed, high-entropy) so ordinary log text stays readable.
func (s *Scrubber) scrubHighEntropy(text string) string {
	return highEntropyTokenRe.ReplaceAllStringFunc(text, func(tok string) string {
		if tok == Redacted {
			return tok
		}
		if looksLikeSecretToken(tok) {
			return Redacted
		}
		return tok
	})
}

// looksLikeSecretToken applies the conservative heuristic: the token must be
// long, contain a mix of character classes (so it is not an all-lowercase word
// or an all-numeric id), and have high per-character entropy.
func looksLikeSecretToken(tok string) bool {
	if len(tok) < 20 {
		return false
	}
	// A purely-hex token is a git SHA / image digest / hash — a non-sensitive
	// identifier, not a secret. Redacting it only hurts log readability.
	if hexTokenRe.MatchString(tok) {
		return false
	}
	var hasLower, hasUpper, hasDigit bool
	for _, r := range tok {
		switch {
		case r >= 'a' && r <= 'z':
			hasLower = true
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= '0' && r <= '9':
			hasDigit = true
		}
	}
	classes := 0
	for _, c := range []bool{hasLower, hasUpper, hasDigit} {
		if c {
			classes++
		}
	}
	// Require at least two character classes: a random-looking mix, not a plain
	// word or a bare number.
	if classes < 2 {
		return false
	}
	return shannonEntropy(tok) >= 3.5
}

// shannonEntropy returns the per-character Shannon entropy (in bits) of s.
func shannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	counts := make(map[rune]int, len(s))
	for _, r := range s {
		counts[r]++
	}
	n := float64(utf8.RuneCountInString(s))
	var h float64
	for _, c := range counts {
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}
