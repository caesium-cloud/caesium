package metricsutil

import (
	"reflect"
	"testing"
)

func TestMatchingCounterSamplesPreservesGrammarAndRawOrder(t *testing.T) {
	text := "\n# m comment\nother{a=\"x\",b=\"y\"} 9\nm{a=\"x\"} 8\nm{a=\"x\",b=\"y\"}\nm{a=\"x\",b=\"y\"} bad\nm_suffix{b=\"y\",a=\"x\"}   2 \nm{a=\"x\",b=\"y\"} 3\n"
	got := MatchingCounterSamples(text, "m", map[string]string{"a": "x", "b": "y"})
	want := []string{"bad", "  2", "3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%q, want %q", got, want)
	}
	if got := MatchingCounterSamples(text, "missing", nil); len(got) != 0 {
		t.Fatal(got)
	}
}
