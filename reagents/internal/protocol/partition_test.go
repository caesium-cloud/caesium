package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func fp(seed string) string {
	return "sha256:" + strings.Repeat(seed, 64/len(seed))
}

func TestPartitionsEmitsSortedObjectForm(t *testing.T) {
	e, out, _ := newTestEmitter()
	err := e.Partitions([]Partition{
		{Key: "app-web", Fingerprint: fp("c"), DependsOn: []string{"network"}, Attributes: map[string]string{"root": "stacks/app-web"}},
		{Key: "network", Fingerprint: fp("a"), Attributes: map[string]string{"root": "stacks/network"}},
		{Key: "account", Fingerprint: fp("b"), DependsOn: []string{"network"}, Attributes: map[string]string{"root": "stacks/account"}},
	})
	if err != nil {
		t.Fatalf("Partitions: %v", err)
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	line := strings.TrimRight(out.String(), "\n")
	payload, ok := strings.CutPrefix(line, PartitionsMarker+" ")
	if !ok {
		t.Fatalf("line %q does not carry the partitions marker", line)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", payload, err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d partitions, want 3", len(got))
	}
	wantOrder := []string{"account", "app-web", "network"}
	for i, want := range wantOrder {
		if got[i]["key"] != want {
			t.Fatalf("partition[%d].key = %v, want %s", i, got[i]["key"], want)
		}
	}
	if got[2]["root"] != "stacks/network" {
		t.Fatalf("free-form attribute lost: %v", got[2])
	}
	if _, hasDeps := got[2]["dependsOn"]; hasDeps {
		t.Fatalf("empty dependsOn should be omitted: %v", got[2])
	}
}

func TestPartitionsIsByteStableAcrossInputOrder(t *testing.T) {
	build := func(parts []Partition) string {
		e, out, _ := newTestEmitter()
		if err := e.Partitions(parts); err != nil {
			t.Fatalf("Partitions: %v", err)
		}
		if err := e.Flush(); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		return out.String()
	}
	a := []Partition{
		{Key: "b", Fingerprint: fp("1"), Attributes: map[string]string{"root": "r/b", "workspace": "prod"}},
		{Key: "a", Fingerprint: fp("2"), Attributes: map[string]string{"workspace": "prod", "root": "r/a"}},
	}
	b := []Partition{a[1], a[0]}
	if build(a) != build(b) {
		t.Fatalf("marker is not order-stable:\n%s\n%s", build(a), build(b))
	}
}

func TestPartitionsRejectsContractViolations(t *testing.T) {
	cases := map[string][]Partition{
		"empty list":      {},
		"empty key":       {{Key: "", Fingerprint: fp("a")}},
		"bad fingerprint": {{Key: "a", Fingerprint: "abc"}},
		// An absent fingerprint is the dangerous one: it validates as
		// "well-formed" under a non-empty-only check and then produces fan-out
		// instances whose cache identity carries no unit content at all.
		"empty fingerprint": {{Key: "a", Fingerprint: ""}},
		"empty fingerprint among valid ones": {
			{Key: "a", Fingerprint: fp("a")},
			{Key: "b", Fingerprint: ""},
		},
		"duplicate key": {{Key: "a", Fingerprint: fp("a")}, {Key: "a", Fingerprint: fp("b")}},
		"dangling dependsOn": {
			{Key: "a", Fingerprint: fp("a"), DependsOn: []string{"ghost"}},
		},
		"self dependency": {{Key: "a", Fingerprint: fp("a"), DependsOn: []string{"a"}}},
		"cycle": {
			{Key: "a", Fingerprint: fp("a"), DependsOn: []string{"b"}},
			{Key: "b", Fingerprint: fp("b"), DependsOn: []string{"a"}},
		},
		"reserved attribute name": {
			{Key: "a", Fingerprint: fp("a"), Attributes: map[string]string{"fingerprint": "x"}},
		},
		"newline in attribute": {
			{Key: "a", Fingerprint: fp("a"), Attributes: map[string]string{"root": "x\ny"}},
		},
		"duplicate dependsOn entry": {
			{Key: "a", Fingerprint: fp("a"), DependsOn: []string{"b", "b"}},
			{Key: "b", Fingerprint: fp("b")},
		},
	}
	for name, parts := range cases {
		t.Run(name, func(t *testing.T) {
			e, out, _ := newTestEmitter()
			if err := e.Partitions(parts); err == nil {
				t.Fatalf("Partitions(%v) = nil error, want rejection", parts)
			}
			if err := e.Flush(); err != nil {
				t.Fatalf("Flush: %v", err)
			}
			if out.Len() != 0 {
				t.Fatalf("rejected partitions still wrote %q", out.String())
			}
		})
	}
}

func TestPartitionsRejectsTooManyAttributes(t *testing.T) {
	attrs := make(map[string]string, MaxPartitionAttributes+1)
	for i := range MaxPartitionAttributes + 1 {
		attrs[string(rune('a'+i))] = "v"
	}
	e, _, _ := newTestEmitter()
	if err := e.Partitions([]Partition{{Key: "a", Fingerprint: fp("a"), Attributes: attrs}}); err == nil {
		t.Fatal("Partitions with too many attributes = nil error")
	}
}

func TestPartitionsRejectsOversizeObject(t *testing.T) {
	e, _, _ := newTestEmitter()
	err := e.Partitions([]Partition{{
		Key:         "a",
		Fingerprint: fp("a"),
		Attributes:  map[string]string{"root": strings.Repeat("x", MaxPartitionObjectBytes)},
	}})
	if err == nil || !strings.Contains(err.Error(), "max") {
		t.Fatalf("oversize partition = %v, want a size rejection", err)
	}
}

func TestSortedKeys(t *testing.T) {
	got := SortedKeys(map[string]int{"c": 0, "a": 0, "b": 0})
	if strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("SortedKeys = %v", got)
	}
}
