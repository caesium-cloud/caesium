package env

import "testing"

func TestParseByteSize(t *testing.T) {
	t.Parallel()

	cases := map[string]int64{
		"8":    8,
		"8B":   8,
		"1KB":  1_000,
		"1MB":  1_000_000,
		"2MiB": 2 << 20,
	}

	for input, expected := range cases {
		got, err := parseByteSize(input)
		if err != nil {
			t.Fatalf("parseByteSize(%q) unexpected error: %v", input, err)
		}
		if got != expected {
			t.Fatalf("parseByteSize(%q) = %d, want %d", input, got, expected)
		}
	}
}
