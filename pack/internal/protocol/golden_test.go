package protocol

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// updateGolden regenerates testdata/protocol/markers.golden instead of
// asserting against it: `go test ./internal/protocol/ -update`.
var updateGolden = flag.Bool("update", false, "rewrite the golden marker file")

// goldenPath is the file this package writes and the ROOT module reads back.
//
// The pack cannot import Caesium and Caesium cannot import the pack — the
// contract between them is a handful of stdout lines, which means nothing in
// either module's type system stops the two from drifting apart. This file is
// the seam: this test pins the exact bytes the emitter produces, and a test in
// the root module (internal/guardrails) feeds those same bytes through
// pkg/task's real parser. Change the emitter and this test fails; change the
// parser and the other one does.
const goldenPath = "../../testdata/protocol/markers.golden"

// goldenMarkers emits one of every marker the pack speaks, using the shapes the
// Terraform binding actually produces: a discover output row with per-input
// digests, a multi-root partition list with a fingerprint, an ordering edge and
// the free-form `root` attribute, a checkout output row, a large-object
// reference, and a branch selection.
func goldenMarkers(t *testing.T) string {
	t.Helper()

	var out bytes.Buffer
	e := New("golden", &out, &out)

	if err := e.Output(map[string]string{
		"commit":     "6f1a2b3c4d5e60718293a4b5c6d7e8f901234567",
		"treeDigest": DigestPrefix + "1111111111111111111111111111111111111111111111111111111111111111",
		"path":       "/src",
	}); err != nil {
		t.Fatalf("Output: %v", err)
	}
	if err := e.Output(map[string]string{
		"fingerprint":     DigestPrefix + "2222222222222222222222222222222222222222222222222222222222222222",
		"input_root":      DigestPrefix + "3333333333333333333333333333333333333333333333333333333333333333",
		"input_tags":      DigestPrefix + "4444444444444444444444444444444444444444444444444444444444444444",
		"input_workspace": DigestPrefix + "5555555555555555555555555555555555555555555555555555555555555555",
	}); err != nil {
		t.Fatalf("Output: %v", err)
	}
	if err := e.Partitions([]Partition{
		{
			Key:         "network",
			Fingerprint: DigestPrefix + "6666666666666666666666666666666666666666666666666666666666666666",
			Attributes:  map[string]string{"root": "network"},
		},
		{
			Key:         "app-web",
			Fingerprint: DigestPrefix + "7777777777777777777777777777777777777777777777777777777777777777",
			DependsOn:   []string{"network"},
			Attributes:  map[string]string{"root": "app-web"},
		},
	}); err != nil {
		t.Fatalf("Partitions: %v", err)
	}
	if err := e.OutputRefDigest(
		"proposal_artifact",
		"/src/stacks/network/tf.plan",
		DigestPrefix+"8888888888888888888888888888888888888888888888888888888888888888",
		4096,
	); err != nil {
		t.Fatalf("OutputRefDigest: %v", err)
	}
	if err := e.Branch("apply"); err != nil {
		t.Fatalf("Branch: %v", err)
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	return out.String()
}

func TestGoldenMarkersMatchTheCheckedInFile(t *testing.T) {
	got := goldenMarkers(t)

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("rewrote %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (regenerate with `go test ./internal/protocol/ -update`): %v", err)
	}
	if got != string(want) {
		t.Fatalf("the emitted markers no longer match %s.\n got: %q\nwant: %q\n"+
			"If this change is intended, regenerate with `go test ./internal/protocol/ -update` — "+
			"and note that the root module parses this same file, so a shape change is a contract change.",
			goldenPath, got, want)
	}
}
