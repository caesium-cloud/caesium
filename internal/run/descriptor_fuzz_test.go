package run

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/google/uuid"
)

// TaskExecutionDescriptor (internal/models/run.go) is the "frozen descriptor":
// the immutable, per-TaskRun runtime envelope captured once at dispatch time
// and read back verbatim by every later consumer — quarantined replay
// (internal/replay), deadline lookups (deadline.go), fan-out recovery
// (owner_topology.go), the worker's image-identity gate
// (runtime_executor.go), and the public /tasks/:task/descriptor read surface
// (store.go). Every one of those call sites runs the SAME two-step gate on the
// stored bytes: json.Unmarshal into models.TaskExecutionDescriptor, then
// reject anything whose SchemaVersion isn't
// models.TaskExecutionDescriptorSchemaVersion. None of them can be driven
// hermetically (they all load the descriptor off a *Store row), so this file
// exercises that shared gate directly against the real exported type and the
// real exported constant — not a hand-rolled proxy of it — which is what
// keeps "every consumer decodes it the same way" true for real.

// validDescriptorSeeds are real *encoded* TaskExecutionDescriptor values, from
// the trivial zero-value envelope through one carrying nested container specs,
// secret refs and a fan-out record — so the corpus starts from bytes shaped
// like what the product actually writes, not just hand-typed JSON fragments.
func validDescriptorSeeds() [][]byte {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	taskID := uuid.MustParse("00000000-0000-0000-0000-0000000000a1")
	jobID := uuid.MustParse("00000000-0000-0000-0000-0000000000b2")

	minimal := models.TaskExecutionDescriptor{
		SchemaVersion: models.TaskExecutionDescriptorSchemaVersion,
		CapturedAt:    now,
	}

	full := models.TaskExecutionDescriptor{
		SchemaVersion: models.TaskExecutionDescriptorSchemaVersion,
		CapturedAt:    now,
		Baseline: models.TaskExecutionBaseline{
			JobID:         jobID,
			JobAlias:      "nightly-etl",
			TaskID:        taskID,
			TaskName:      "extract",
			ReplaySafe:    true,
			ComputedHash:  "sha256:deadbeef",
			EffectiveHash: "sha256:deadbeef",
		},
		DAG: models.TaskExecutionDAG{
			Predecessors: []models.TaskExecutionEdgeRef{{TaskID: jobID, TaskName: "upstream"}},
			TriggerRule:  "all_success",
			TaskPosition: 1,
		},
		Run: models.TaskExecutionRun{
			Params: map[string]string{"env": "prod"},
		},
		Runtime: models.TaskExecutionRuntime{
			Engine:  "docker",
			Image:   "alpine:3.23",
			Command: []string{"/bin/sh", "-c", "echo hi"},
		},
		Timing: models.TaskExecutionTiming{
			TaskTimeout: 30 * time.Second,
			RunTimeout:  time.Hour,
		},
		Cache: models.TaskExecutionCache{
			Enabled: true,
			TTL:     time.Minute,
		},
		Schema: models.TaskExecutionSchema{
			ValidationMode: "warn",
		},
		Job: models.TaskExecutionJob{
			MaxParallelTasks: 4,
			Labels:           map[string]string{"team": "data"},
		},
		ContainerSpec: container.Spec{
			Env:     map[string]string{"FOO": "bar"},
			WorkDir: "/work",
			Kubernetes: &container.KubernetesSpec{
				ServiceAccountName: "runner",
			},
		},
		KubernetesSpec: &container.KubernetesSpec{
			ServiceAccountName: "runner",
			PodAnnotations:     map[string]string{"a": "b"},
		},
		SecretRefs: []models.TaskExecutionSecretRef{
			{Ref: "secret://env/API_KEY", EnvKey: "API_KEY", Provider: "env", Verifiable: true},
		},
		FanOut: &models.TaskExecutionFanOut{
			Partitions: nil,
		},
	}

	var out [][]byte
	for _, d := range []models.TaskExecutionDescriptor{minimal, full} {
		b, err := json.Marshal(d)
		if err != nil {
			panic(err)
		}
		out = append(out, b)
	}
	return out
}

// FuzzTaskExecutionDescriptorRoundTrip drives the real json.Unmarshal /
// json.Marshal pair over models.TaskExecutionDescriptor — the exact codec
// every real consumer relies on — against adversarial bytes.
//
// Properties (never "did not panic" alone):
//
//  1. Round trip is a fixed point: for any bytes that decode successfully,
//     re-encoding and re-decoding the result must reproduce an identical
//     value. A descriptor that failed this would mean the product's own
//     mutate-read-modify-write path (mutateTaskExecutionDescriptor in
//     store.go, which decodes, mutates, and re-encodes) could silently drift
//     the stored envelope on every unrelated update.
//  2. The decode+version gate is deterministic: decoding the same bytes twice
//     must reach the same accept/reject verdict every time, and that verdict
//     must survive the round trip in (1) — this is the "validated input is
//     accepted consistently" claim for the one gate every real call site
//     (store.go x2, deadline.go x3, owner_topology.go, runtime_executor.go)
//     runs textually identically.
func FuzzTaskExecutionDescriptorRoundTrip(f *testing.F) {
	for _, b := range validDescriptorSeeds() {
		f.Add(b)
	}
	f.Add([]byte("{}"))
	f.Add([]byte("null"))
	f.Add([]byte(""))
	f.Add([]byte(`{"schemaVersion":1}`))
	f.Add([]byte(`{"schemaVersion":2,"baseline":{}}`))
	f.Add([]byte(`{"schemaVersion":1,"containerSpec":{"env":null}}`))
	f.Add([]byte(`{"schemaVersion":1,"secretRefs":[{}]}`))
	f.Add([]byte(`not json at all`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var first models.TaskExecutionDescriptor
		err1 := json.Unmarshal(data, &first)

		// Determinism: decoding identical bytes twice must never disagree.
		var firstAgain models.TaskExecutionDescriptor
		err1Again := json.Unmarshal(data, &firstAgain)
		if (err1 == nil) != (err1Again == nil) {
			t.Fatalf("decoding %q twice disagreed on success: %v then %v", data, err1, err1Again)
		}
		if err1 == nil && !reflect.DeepEqual(first, firstAgain) {
			t.Fatalf("decoding %q twice produced different values:\n%+v\n%+v", data, first, firstAgain)
		}

		if err1 != nil {
			// A rejected blob's fate is exactly what every real call site does:
			// treat it as absent/invalid and stop. Nothing further to check.
			return
		}

		accepted := first.SchemaVersion == models.TaskExecutionDescriptorSchemaVersion

		// Round-trip fixed point, checked at the BYTE level rather than by
		// comparing decoded Go values: encoding/json's `omitempty` collapses an
		// explicit empty slice/map in the input into a nil field once it has
		// been re-marshaled, so first (decoded straight from arbitrary fuzz
		// bytes) can legitimately hold a non-nil empty collection that no
		// longer round-trips to an EQUAL Go value — that is a property of
		// Go's JSON codec, not a descriptor defect. What must hold, and is a
		// real fixed point, is that re-marshaling a value produced by this
		// codec is stable: Marshal(Unmarshal(Marshal(first))) ==
		// Marshal(Unmarshal(data)).
		reencoded1, err := json.Marshal(first)
		if err != nil {
			t.Fatalf("Marshal of a successfully decoded descriptor failed: %v\nvalue=%+v", err, first)
		}
		var second models.TaskExecutionDescriptor
		if err := json.Unmarshal(reencoded1, &second); err != nil {
			t.Fatalf("re-decoding Marshal(Unmarshal(data)) failed: %v\nintermediate=%s", err, reencoded1)
		}
		reencoded2, err := json.Marshal(second)
		if err != nil {
			t.Fatalf("Marshal of a re-decoded descriptor failed: %v\nvalue=%+v", err, second)
		}
		if !bytes.Equal(reencoded1, reencoded2) {
			t.Fatalf("decode -> encode -> decode -> encode is not a fixed point for %q:\npass1=%s\npass2=%s", data, reencoded1, reencoded2)
		}

		// The version gate must survive the round trip: a blob every consumer
		// would accept (or reject) must still be accepted (or rejected) after
		// being written back out in the product's own encoding.
		acceptedAgain := second.SchemaVersion == models.TaskExecutionDescriptorSchemaVersion
		if accepted != acceptedAgain {
			t.Fatalf("SchemaVersion gate did not survive the round trip for %q: before=%v after=%v", data, accepted, acceptedAgain)
		}
	})
}

// FuzzMergeDescriptorSecretRefs targets mergeDescriptorSecretRefs (store.go),
// the pure function every descriptor secret-ref update goes through
// (UpdateTaskExecutionDescriptorSecretRefs). It is hermetic on its own: no
// Store, no descriptor envelope, just two slices.
//
// Properties:
//   - Conservation: every existing entry whose key does not appear in updates
//     is still present (by key) in the output.
//   - Idempotence: merging the same updates a second time is a no-op fixed
//     point — applying an update twice must reach the same slice content as
//     applying it once, because a repeat delivery of the same descriptor
//     mutation must not keep perturbing the stored envelope forever. This is
//     checked for arbitrary existing/updates input, including inputs that
//     already carry a duplicate (envKey, ref) key — which this fuzz target
//     found the function does NOT itself clean up in either merge path (the
//     len(existing)==0 fast path additionally fails to dedupe duplicate keys
//     WITHIN updates; filed as
//     https://github.com/caesium-cloud/caesium/issues/549, currently
//     unreachable in production since both real callers build updates by
//     ranging over a map[string]string keyed by EnvKey). "No duplicate keys
//     in the output" is therefore NOT asserted here — it is not a property
//     this function actually guarantees for arbitrary input — but idempotence
//     provably still holds regardless: each update key resolves to a stable
//     index on the first application (the general path's index always
//     tracks the LAST occurrence of a repeated key), and reapplying the same
//     updates re-targets that same index with the same value.
func FuzzMergeDescriptorSecretRefs(f *testing.F) {
	seed := func(existing, updates []models.TaskExecutionSecretRef) []byte {
		b, err := json.Marshal([2][]models.TaskExecutionSecretRef{existing, updates})
		if err != nil {
			panic(err)
		}
		return b
	}
	f.Add(seed(nil, nil))
	f.Add(seed(
		[]models.TaskExecutionSecretRef{{Ref: "secret://env/A", EnvKey: "A"}},
		[]models.TaskExecutionSecretRef{{Ref: "secret://env/A", EnvKey: "A", Verifiable: true}},
	))
	f.Add(seed(
		[]models.TaskExecutionSecretRef{{Ref: "secret://env/A", EnvKey: "A"}, {Ref: "secret://vault/B", EnvKey: "B"}},
		[]models.TaskExecutionSecretRef{{Ref: "secret://k8s/C", EnvKey: "C"}},
	))
	f.Add([]byte(`[[],[{"envKey":"","ref":""},{"envKey":"","ref":""}]]`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var pair [2][]models.TaskExecutionSecretRef
		if err := json.Unmarshal(data, &pair); err != nil {
			// The property under test is the merge, not the JSON grammar; an
			// input that doesn't even decode into two ref slices has nothing to
			// merge. Structural reason to skip, matching this repo's existing
			// fuzz style (internal/trigger/cron/fuzz_test.go).
			return
		}
		existing, updates := pair[0], pair[1]

		merged := mergeDescriptorSecretRefs(existing, updates)

		key := func(r models.TaskExecutionSecretRef) string { return r.EnvKey + "\x00" + r.Ref }

		seen := make(map[string]bool, len(merged))
		for _, r := range merged {
			seen[key(r)] = true
		}

		updateKeys := make(map[string]bool, len(updates))
		for _, r := range updates {
			updateKeys[key(r)] = true
		}
		for _, r := range existing {
			if updateKeys[key(r)] {
				continue // superseded by an update; conservation does not apply
			}
			if !seen[key(r)] {
				t.Fatalf("merge dropped existing entry %+v that no update touched (existing=%+v updates=%+v)", r, existing, updates)
			}
		}

		again := mergeDescriptorSecretRefs(merged, updates)
		if !reflect.DeepEqual(merged, again) {
			t.Fatalf("merging the same updates twice is not idempotent:\nonce=%+v\ntwice=%+v", merged, again)
		}
	})
}
