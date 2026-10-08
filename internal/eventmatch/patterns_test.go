package eventmatch

import (
	"encoding/json"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
)

func TestParseTriggerEventPatterns(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cfg   map[string]any
		err   string
		count int
	}{
		{"missing", nil, "", 0}, {"null", map[string]any{"events": nil}, "", 0}, {"empty", map[string]any{"events": []any{}}, "", 0},
		{"list", map[string]any{"events": 1}, "trigger.configuration.events must be a list", 0},
		{"object", map[string]any{"events": []any{1}}, "trigger.configuration.events[0] must be an object", 0},
		{"filter", map[string]any{"events": []any{map[string]any{"filter": 1}}}, "trigger.configuration.events[0].filter must be an object", 0},
		{"fields", map[string]any{"events": []any{map[string]any{"type": 1, "source": false, "filter": map[string]any{"ignore": 1, "keep": "value"}}}}, "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := ParseTriggerEventPatterns(tc.cfg)
			if tc.err != "" {
				if err == nil || err.Error() != tc.err {
					t.Fatalf("error %v", err)
				}
				return
			}
			if err != nil || len(p) != tc.count {
				t.Fatalf("%v %v", p, err)
			}
			if tc.name == "fields" && (p[0].Type != "" || p[0].Source != "" || len(p[0].Filter) != 1 || p[0].Filter["keep"] != "value") {
				t.Fatal(p)
			}
		})
	}
}
func TestSharedEventValuePolicies(t *testing.T) {
	data := []byte(`{"n":1.00,"list":[{"name":"ok"}],"null":null} trailing`)
	for _, tc := range []struct {
		path, want string
		ok         bool
	}{{"$.n", "1.00", true}, {"$.list[0].name", "ok", true}, {"$.null", "", false}, {"$.list[-1]", "", false}, {"$.a..b", "", false}} {
		got, ok := ResolveJSONPathBytes(data, tc.path)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("%q=%q,%t", tc.path, got, ok)
		}
	}
	if got, _ := StringifyJSONValue(float64(1)); got != "1" {
		t.Fatal(got)
	}
	if got, _ := StringifyJSONValue(json.Number("1.00")); got != "1.00" {
		t.Fatal(got)
	}
	evt := &models.IngestedEvent{Type: "run_completed", Source: " caesium ", Data: data}
	if !(EventPattern{Type: "run_*", Source: "caesium", Filter: map[string]string{"n": "1.00"}}).Matches(evt) {
		t.Fatal("matcher lost decoder semantics")
	}
	for _, pattern := range []string{"", "[", "task_*"} {
		if MatchesEventType(pattern, evt.Type) {
			t.Fatalf("unexpected match %q", pattern)
		}
	}
}

func TestEventDecodersRejectEmptyAndMalformedPayloads(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{name: "empty"},
		{name: "malformed", data: []byte(`{"n":`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := ExtractField(tc.data, "n"); ok {
				t.Fatal("ExtractField accepted an undecodable payload")
			}
			if _, ok := ResolveJSONPathBytes(tc.data, "$"); ok {
				t.Fatal("ResolveJSONPathBytes accepted an undecodable payload")
			}
			evt := &models.IngestedEvent{Type: "run_completed", Data: tc.data}
			if (EventPattern{Type: "run_completed", Filter: map[string]string{"n": "1"}}).Matches(evt) {
				t.Fatal("EventPattern matched an undecodable payload")
			}
		})
	}
}
