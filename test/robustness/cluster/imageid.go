package cluster

import (
	"strings"
	"unicode"
)

func NormalizeImageDigest(raw string) string {
	raw = strings.TrimSpace(raw)
	if i := strings.LastIndex(raw, "sha256:"); i >= 0 {
		hex := raw[i+len("sha256:"):]
		if j := strings.IndexAny(hex, " \t@,;"); j >= 0 {
			hex = hex[:j]
		}
		hex = strings.TrimSpace(hex)
		if hex != "" {
			return "sha256:" + strings.ToLower(hex)
		}
	}
	return raw
}

func SplitImageIdentities(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || unicode.IsSpace(r)
	})
	seen := map[string]struct{}{}
	var out []string
	for _, f := range fields {
		n := NormalizeImageDigest(f)
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}

func ImageIDMatchesCandidate(imageID, candidate string) bool {
	got := NormalizeImageDigest(imageID)
	if got == "" {
		return false
	}
	for _, want := range SplitImageIdentities(candidate) {
		if want != "" && got == want {
			return true
		}
	}
	return false
}
