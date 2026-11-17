package reproduce

import (
	"strings"
	"testing"
)

func TestFormatMiniWhyOmitsSecretValues(t *testing.T) {
	const secret = "super-secret-value"
	const token = "sk-live-secret"
	line := formatMiniWhy(&miniWhy{
		BaselineKind:    "prior_run",
		ComparedToRunID: "11111111-1111-1111-1111-111111111111",
		Fields: []miniWhyField{
			{
				Field:    "env.PASSWORD",
				Change:   "changed",
				Before:   secret,
				After:    "other-secret",
				Redacted: true,
			},
			{
				Field:  "runParams.api_token",
				Change: "added",
				After:  token,
			},
			{
				Field:  "env.SECRET_ENV",
				Change: "changed",
				Before: "secret://env/OLD",
				After:  "secret://env/NEW",
			},
			{
				Field:  "runParams.flavor",
				Change: "changed",
				Before: "vanilla",
				After:  "chocolate",
			},
		},
	})

	if strings.Contains(line, secret) || strings.Contains(line, "other-secret") || strings.Contains(line, token) {
		t.Fatalf("mini-why leaked a secret value: %s", line)
	}
	for _, want := range []string{
		"env.PASSWORD changed (redacted)",
		"runParams.api_token added (redacted)",
		"secret://env/OLD",
		"secret://env/NEW",
		"runParams.flavor changed vanilla→chocolate",
		"prior run 11111111-1111-1111-1111-111111111111",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("mini-why %q missing %q", line, want)
		}
	}
}

func TestFormatMiniWhyNoPriorRun(t *testing.T) {
	if got := formatMiniWhy(&miniWhy{BaselineKind: "none"}); got != "mini-why: no prior run to compare against" {
		t.Fatalf("got %q", got)
	}
	if got := formatMiniWhy(nil); got != "" {
		t.Fatalf("nil mini-why = %q", got)
	}
}
