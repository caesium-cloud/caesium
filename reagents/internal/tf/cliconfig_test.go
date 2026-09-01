package tf

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeCacheKeyAcceptsTheEmptyDefault(t *testing.T) {
	for _, raw := range []string{"", "  ", "\t"} {
		got, err := SanitizeCacheKey(raw)
		if err != nil || got != "" {
			t.Fatalf("SanitizeCacheKey(%q) = %q, %v (want empty)", raw, got, err)
		}
	}
}

func TestSanitizeCacheKeyAcceptsSlotNames(t *testing.T) {
	for _, raw := range []string{"deploy", "legacy-providers", "v1.2", "a", "a_b.c-9"} {
		got, err := SanitizeCacheKey(raw)
		if err != nil {
			t.Fatalf("SanitizeCacheKey(%q): %v", raw, err)
		}
		if got != raw {
			t.Fatalf("SanitizeCacheKey(%q) = %q (keys are not rewritten)", raw, got)
		}
	}
}

func TestSanitizeCacheKeyRejectsUnsafeOrReservedNames(t *testing.T) {
	cases := map[string]string{
		"foo/bar":               "single path element",
		`foo\bar`:               "single path element",
		"../etc":                "single path element",
		" deploy ":              "leading or trailing whitespace",
		"deploy\n":              "leading or trailing whitespace",
		"..":                    "must not start with '.'",
		".":                     "must not start with '.'",
		".warm":                 "must not start with '.'",
		"providers":             "reserved",
		"Providers":             "reserved",
		"terraformrc":           "reserved",
		"TerraformRC":           "reserved",
		"providers.tmp.dead":    "staging",
		"Providers.tmp.dead":    "staging",
		"Deploy":                "lower-case letters",
		"-leading-dash":         "not a valid slot name",
		"foo bar":               "not a valid slot name",
		"has$dollar":            "not a valid slot name",
		strings.Repeat("a", 64): "not a valid slot name",
	}
	for raw, want := range cases {
		_, err := SanitizeCacheKey(raw)
		if err == nil {
			t.Fatalf("SanitizeCacheKey(%q) accepted a key it must reject", raw)
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("SanitizeCacheKey(%q) error %q does not name %q", raw, err, want)
		}
	}
}

func TestCLIConfigFileUnkeyedSlotIsTheHistoricalPath(t *testing.T) {
	got := CLIConfigFile(DefaultCacheDir, "")
	want := filepath.Join(DefaultCacheDir, TerraformRCName)
	if got != want {
		t.Fatalf("CLIConfigFile(%q, %q) = %q, want %q", DefaultCacheDir, "", got, want)
	}
}

func TestCLIConfigFileKeyedSlotNestsUnderTheKey(t *testing.T) {
	got := CLIConfigFile(DefaultCacheDir, "deploy")
	want := filepath.Join(DefaultCacheDir, "deploy", TerraformRCName)
	if got != want {
		t.Fatalf("CLIConfigFile(%q, %q) = %q, want %q", DefaultCacheDir, "deploy", got, want)
	}
}
