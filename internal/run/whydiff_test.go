package run

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/cache"
	"github.com/caesium-cloud/caesium/pkg/container"
)

// blobFor canonicalizes a HashInput the same way the write-path does (A2), so the
// diff tests exercise real, version-pinned blobs rather than hand-rolled JSON.
func blobFor(t *testing.T, h cache.HashInput) []byte {
	t.Helper()
	data, err := h.CanonicalJSON(h.Compute())
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	return data
}

func envDigest(v string) string {
	sum := sha256.Sum256([]byte(v))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func findChange(changes []FieldChange, field string) (FieldChange, bool) {
	for _, c := range changes {
		if c.Field == field {
			return c, true
		}
	}
	return FieldChange{}, false
}

func TestDiff_IdenticalBlobsHashEqualNoChanges(t *testing.T) {
	h := cache.HashInput{
		JobAlias: "etl", TaskName: "transform",
		Image:   "alpine:3.23",
		Command: []string{"sh", "-c", "echo hi"},
		Env:     map[string]string{"FOO": "bar"},
	}
	blob := blobFor(t, h)

	d, err := DiffHashInputBlobs(blob, blob)
	if err != nil {
		t.Fatalf("DiffHashInputBlobs: %v", err)
	}
	if !d.HashEqual {
		t.Errorf("expected HashEqual=true for identical blobs")
	}
	if len(d.Changes) != 0 {
		t.Errorf("expected no changes, got %+v", d.Changes)
	}
	if d.Degraded != "" {
		t.Errorf("unexpected degraded: %q", d.Degraded)
	}
}

func TestDiff_PredecessorOutputChangeIsHeadline(t *testing.T) {
	base := cache.HashInput{
		TaskName: "load",
		Image:    "alpine:3.23",
		PredecessorOutputs: map[string]map[string]string{
			"extract": {"row_count": "1200000"},
		},
	}
	subject := cache.HashInput{
		TaskName: "load",
		Image:    "alpine:3.23",
		PredecessorOutputs: map[string]map[string]string{
			"extract": {"row_count": "1400000"},
		},
	}

	d, err := DiffHashInputBlobs(blobFor(t, subject), blobFor(t, base))
	if err != nil {
		t.Fatalf("DiffHashInputBlobs: %v", err)
	}
	if d.HashEqual {
		t.Fatalf("expected hashes to differ")
	}
	c, ok := findChange(d.Changes, "predecessorOutputs.extract.row_count")
	if !ok {
		t.Fatalf("expected predecessorOutputs.extract.row_count change, got %+v", d.Changes)
	}
	if c.Before != "1200000" || c.After != "1400000" {
		t.Errorf("before/after = %q/%q, want 1200000/1400000", c.Before, c.After)
	}
	if c.Redacted {
		t.Errorf("predecessor outputs are data-contract values, must not be redacted")
	}
	// Only the one field should differ.
	if len(d.Changes) != 1 {
		t.Errorf("expected exactly 1 change, got %+v", d.Changes)
	}
}

func TestDiff_EnvValueRedactedDigestComparison(t *testing.T) {
	base := cache.HashInput{TaskName: "t", Env: map[string]string{"DATABASE_URL": "postgres://old"}}
	subject := cache.HashInput{TaskName: "t", Env: map[string]string{"DATABASE_URL": "postgres://new"}}

	d, err := DiffHashInputBlobs(blobFor(t, subject), blobFor(t, base))
	if err != nil {
		t.Fatalf("DiffHashInputBlobs: %v", err)
	}
	c, ok := findChange(d.Changes, "env.DATABASE_URL")
	if !ok {
		t.Fatalf("expected env.DATABASE_URL change, got %+v", d.Changes)
	}
	if !c.Redacted {
		t.Errorf("expected literal env value diff to be marked Redacted")
	}
	if c.Before != envDigest("postgres://old") || c.After != envDigest("postgres://new") {
		t.Errorf("expected digests, got before=%q after=%q", c.Before, c.After)
	}
	// The plaintext must never appear in the diff.
	raw, _ := json.Marshal(d)
	if strings.Contains(string(raw), "postgres://old") || strings.Contains(string(raw), "postgres://new") {
		t.Errorf("plaintext env value leaked into diff JSON: %s", raw)
	}
}

func TestDiff_SecretRefShownVerbatim(t *testing.T) {
	base := cache.HashInput{TaskName: "t", Env: map[string]string{"TOKEN": "secret://vault/db/pass"}}
	subject := cache.HashInput{TaskName: "t", Env: map[string]string{"TOKEN": "secret://vault/db/pass-v2"}}

	d, err := DiffHashInputBlobs(blobFor(t, subject), blobFor(t, base))
	if err != nil {
		t.Fatalf("DiffHashInputBlobs: %v", err)
	}
	c, ok := findChange(d.Changes, "env.TOKEN")
	if !ok {
		t.Fatalf("expected env.TOKEN change, got %+v", d.Changes)
	}
	if c.Redacted {
		t.Errorf("secret:// references are non-secret pointers; must not be marked Redacted")
	}
	if c.Before != "secret://vault/db/pass" || c.After != "secret://vault/db/pass-v2" {
		t.Errorf("expected verbatim secret refs, got before=%q after=%q", c.Before, c.After)
	}
}

func TestDiff_AddedAndRemovedFields(t *testing.T) {
	base := cache.HashInput{
		TaskName:  "t",
		Env:       map[string]string{"KEEP": "x", "GONE": "y"},
		RunParams: map[string]string{"region": "us"},
	}
	subject := cache.HashInput{
		TaskName:  "t",
		Env:       map[string]string{"KEEP": "x", "NEW": "z"},
		RunParams: map[string]string{"region": "us", "tier": "gold"},
	}

	d, err := DiffHashInputBlobs(blobFor(t, subject), blobFor(t, base))
	if err != nil {
		t.Fatalf("DiffHashInputBlobs: %v", err)
	}

	gone, ok := findChange(d.Changes, "env.GONE")
	if !ok || !gone.Removed {
		t.Errorf("expected env.GONE removed, got %+v", d.Changes)
	}
	added, ok := findChange(d.Changes, "env.NEW")
	if !ok || !added.Added {
		t.Errorf("expected env.NEW added, got %+v", d.Changes)
	}
	param, ok := findChange(d.Changes, "runParams.tier")
	if !ok || !param.Added || param.After != "gold" {
		t.Errorf("expected runParams.tier added=gold, got %+v", param)
	}
	if _, ok := findChange(d.Changes, "env.KEEP"); ok {
		t.Errorf("env.KEEP unchanged should not appear")
	}
}

func TestDiff_ScalarImageAndCommand(t *testing.T) {
	// Two distinct canonical pinned images so the change is real but the repo
	// image-pinning guardrail stays satisfied.
	const baseImage, subjectImage = "alpine:3.23", "busybox:1.36.1"
	base := cache.HashInput{TaskName: "t", Image: baseImage, Command: []string{"echo", "a"}}
	subject := cache.HashInput{TaskName: "t", Image: subjectImage, Command: []string{"echo", "b"}}

	d, err := DiffHashInputBlobs(blobFor(t, subject), blobFor(t, base))
	if err != nil {
		t.Fatalf("DiffHashInputBlobs: %v", err)
	}
	img, ok := findChange(d.Changes, "image")
	if !ok || img.Before != baseImage || img.After != subjectImage {
		t.Errorf("expected image change %s->%s, got %+v", baseImage, subjectImage, img)
	}
	if _, ok := findChange(d.Changes, "command"); !ok {
		t.Errorf("expected command change, got %+v", d.Changes)
	}
	// Changes must be sorted by field for determinism.
	for i := 1; i < len(d.Changes); i++ {
		if d.Changes[i-1].Field > d.Changes[i].Field {
			t.Errorf("changes not sorted: %s before %s", d.Changes[i-1].Field, d.Changes[i].Field)
		}
	}
}

func TestDiff_StructuralMountChange(t *testing.T) {
	base := cache.HashInput{
		TaskName: "t",
		Mounts:   []container.Mount{{Source: "/a", Target: "/in", Type: "bind"}},
	}
	subject := cache.HashInput{
		TaskName: "t",
		Mounts:   []container.Mount{{Source: "/b", Target: "/in", Type: "bind"}},
	}

	d, err := DiffHashInputBlobs(blobFor(t, subject), blobFor(t, base))
	if err != nil {
		t.Fatalf("DiffHashInputBlobs: %v", err)
	}
	c, ok := findChange(d.Changes, "mounts")
	if !ok {
		t.Fatalf("expected mounts structural change, got %+v", d.Changes)
	}
	if c.Kind != fieldStructural {
		t.Errorf("expected structural kind, got %s", c.Kind)
	}
}

func TestDiff_PredecessorHashSetChange(t *testing.T) {
	base := cache.HashInput{TaskName: "t", PredecessorHashes: []string{"h1", "h2"}}
	subject := cache.HashInput{TaskName: "t", PredecessorHashes: []string{"h1", "h3"}}

	d, err := DiffHashInputBlobs(blobFor(t, subject), blobFor(t, base))
	if err != nil {
		t.Fatalf("DiffHashInputBlobs: %v", err)
	}
	var removed, added bool
	for _, c := range d.Changes {
		if c.Field != "predecessorHashes" {
			continue
		}
		if c.Removed && c.Before == "h2" {
			removed = true
		}
		if c.Added && c.After == "h3" {
			added = true
		}
	}
	if !removed || !added {
		t.Errorf("expected h2 removed and h3 added, got %+v", d.Changes)
	}
}

func TestDiff_MissingBaselineBlobDegrades(t *testing.T) {
	subject := blobFor(t, cache.HashInput{TaskName: "t", Image: "alpine:3.23"})

	d, err := DiffHashInputBlobs(subject, nil)
	if err != nil {
		t.Fatalf("DiffHashInputBlobs: %v", err)
	}
	if d.Degraded == "" {
		t.Errorf("expected a degraded reason when baseline is missing")
	}
	if d.SubjectHash == "" {
		t.Errorf("expected subject hash to still be surfaced, got empty")
	}
	if len(d.Changes) != 0 {
		t.Errorf("expected no field changes when degraded, got %+v", d.Changes)
	}
}

func TestDiff_OversizedBlobDegrades(t *testing.T) {
	// Build a HashInput large enough to trip the 64 KB oversized fallback so the
	// persisted blob carries an oversized marker.
	big := map[string]map[string]string{}
	huge := map[string]string{}
	for i := 0; i < 5000; i++ {
		huge[padKey(i)] = padKey(i)
	}
	big["step"] = huge
	subject := blobFor(t, cache.HashInput{TaskName: "t", PredecessorOutputs: big})
	baseline := blobFor(t, cache.HashInput{TaskName: "t", Image: "alpine:3.23"})

	d, err := DiffHashInputBlobs(subject, baseline)
	if err != nil {
		t.Fatalf("DiffHashInputBlobs: %v", err)
	}
	if d.Degraded == "" {
		t.Errorf("expected degraded when a blob is oversized, got none (changes=%+v)", d.Changes)
	}
	if len(d.Changes) != 0 {
		t.Errorf("expected no field-level changes for an oversized blob")
	}
}

func TestDiff_VersionMismatchDegrades(t *testing.T) {
	// Force a version mismatch by editing the decoded JSON's blobVersion.
	base := blobFor(t, cache.HashInput{TaskName: "t", Image: "a"})
	var m map[string]interface{}
	if err := json.Unmarshal(base, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	m["blobVersion"] = float64(cache.HashInputBlobVersion + 1)
	bumped, _ := json.Marshal(m)

	d, err := DiffHashInputBlobs(bumped, base)
	if err != nil {
		t.Fatalf("DiffHashInputBlobs: %v", err)
	}
	if d.Degraded == "" {
		t.Errorf("expected degraded on version mismatch")
	}
}

func TestDiff_InvalidJSONErrors(t *testing.T) {
	if _, err := DiffHashInputBlobs([]byte("{not json"), []byte(`{"blobVersion":1}`)); err == nil {
		t.Errorf("expected error decoding invalid subject blob")
	}
}

// padKey returns a distinct, multi-byte key per index so a few thousand entries
// reliably push the canonical blob past the 64 KB oversized bound.
func padKey(i int) string {
	return "padding-key-to-inflate-blob-size-" + strconv.Itoa(i)
}
