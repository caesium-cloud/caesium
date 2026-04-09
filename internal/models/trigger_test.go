package models

import "testing"

func TestNormalizedTriggerPath(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"/hooks/run":            "run",
		"/v1/hooks/run":         "run",
		"/hooks/v1/build":       "v1/build",
		"/v1/hooks/v1/build":    "v1/build",
		"hooks/hooks/secondary": "hooks/secondary",
	}

	for input, expected := range cases {
		if got := NormalizedTriggerPath(input); got != expected {
			t.Fatalf("NormalizedTriggerPath(%q) = %q, want %q", input, got, expected)
		}
	}
}
