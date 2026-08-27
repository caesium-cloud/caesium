package guardrails_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/pkg/task"
)

// The pack (pack/, a separate Go module) and Caesium talk to each other only
// through stdout marker lines. Neither module can import the other — that
// separation is deliberate, so Terraform's dependencies stay out of Caesium's
// go.sum — which means nothing in either type system stops the emitter and the
// parser from drifting apart. The pack's own tests prove it emits what it meant
// to; pkg/task's tests prove the parser reads what it expects. Nobody was
// checking that those two are the same thing.
//
// pack/testdata/protocol/markers.golden is the seam. It is written by
// pack/internal/protocol's golden test (which fails if the emitter changes) and
// parsed here by the real pkg/task parser (which fails if the marker shape
// stops being accepted). A change to either side breaks one of the two.
const packMarkersGolden = "pack/testdata/protocol/markers.golden"

func readPackGolden(t *testing.T) string {
	t.Helper()
	path := filepath.Join(repoRoot(t), filepath.FromSlash(packMarkersGolden))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s (regenerate with `cd pack && go test ./internal/protocol/ -update`): %v",
			packMarkersGolden, err)
	}
	return string(data)
}

func TestPackMarkersParseWithTheRealParser(t *testing.T) {
	markers, err := task.ParseMarkers(strings.NewReader(readPackGolden(t)))
	if err != nil {
		t.Fatalf("pkg/task rejected the markers the pack emits: %v", err)
	}

	// --- structured output ------------------------------------------------
	if got := markers.Output["commit"]; got != "6f1a2b3c4d5e60718293a4b5c6d7e8f901234567" {
		t.Fatalf("commit = %q", got)
	}
	if !strings.HasPrefix(markers.Output["treeDigest"], "sha256:") {
		t.Fatalf("treeDigest = %q", markers.Output["treeDigest"])
	}
	// Both output lines merge, so discover's fingerprint and per-input digests
	// survive alongside checkout's.
	if !strings.HasPrefix(markers.Output["fingerprint"], "sha256:") {
		t.Fatalf("fingerprint = %q (keys: %v)", markers.Output["fingerprint"], outputKeys(markers.Output))
	}
	for _, key := range []string{"input_root", "input_tags", "input_workspace"} {
		if !strings.HasPrefix(markers.Output[key], "sha256:") {
			t.Fatalf("%s = %q", key, markers.Output[key])
		}
	}

	// --- output-ref -------------------------------------------------------
	ref, ok := task.DecodeOutputRef(markers.Output["proposal_artifact"])
	if !ok {
		t.Fatalf("the pack's output-ref did not decode: %q", markers.Output["proposal_artifact"])
	}
	if ref.Digest != "sha256:8888888888888888888888888888888888888888888888888888888888888888" {
		t.Fatalf("ref digest = %q", ref.Digest)
	}
	if ref.Size != 4096 {
		t.Fatalf("ref size = %d", ref.Size)
	}
	if ref.Path != "/src/stacks/network/tf.plan" {
		t.Fatalf("ref path = %q", ref.Path)
	}

	// --- branch -----------------------------------------------------------
	if len(markers.Branches) != 1 || markers.Branches[0] != "apply" {
		t.Fatalf("branches = %v", markers.Branches)
	}

	// --- partitions (the object form) -------------------------------------
	// This is the shape with no other coverage: the pack hand-copies pkg/task's
	// reserved-key set and caps, and the fan-out consumer is what has to accept
	// them.
	if len(markers.Partitions) != 2 {
		t.Fatalf("got %d partitions, want 2: %#v", len(markers.Partitions), markers.Partitions)
	}
	byKey := map[string]task.Partition{}
	for _, p := range markers.Partitions {
		byKey[p.Key] = p
	}

	network, ok := byKey["network"]
	if !ok {
		t.Fatalf("partition 'network' missing: %#v", markers.Partitions)
	}
	if network.Fingerprint != "sha256:6666666666666666666666666666666666666666666666666666666666666666" {
		t.Fatalf("network fingerprint = %q — the parser did not keep the per-unit fingerprint", network.Fingerprint)
	}
	if len(network.DependsOn) != 0 {
		t.Fatalf("network dependsOn = %v, want none", network.DependsOn)
	}
	// `root` is not a reserved field, so it must survive as a free-form scalar
	// attribute — that is how tf-runner learns which directory to plan.
	if network.Attributes["root"] != "network" {
		t.Fatalf("network attributes = %v, want root=network", network.Attributes)
	}

	appWeb, ok := byKey["app-web"]
	if !ok {
		t.Fatalf("partition 'app-web' missing: %#v", markers.Partitions)
	}
	if len(appWeb.DependsOn) != 1 || appWeb.DependsOn[0] != "network" {
		t.Fatalf("app-web dependsOn = %v — the parser did not keep the ordering edge", appWeb.DependsOn)
	}
	if appWeb.Attributes["root"] != "app-web" {
		t.Fatalf("app-web attributes = %v", appWeb.Attributes)
	}

	// The graph the scheduler would expand must be valid, not merely parseable:
	// this is the same call the fan-out expansion makes.
	graph, err := task.ValidatePartitionGraph(markers.Partitions)
	if err != nil {
		t.Fatalf("the pack's partition graph is not schedulable: %v", err)
	}
	if graph == nil {
		t.Fatal("ValidatePartitionGraph returned no graph")
	}
}

// TestPackPartitionCapsMatchTheParser pins the four limits the pack duplicates
// because it cannot import them. A cap that drifts wider in the pack means
// discover emits a marker the server then rejects — a task failure with no
// fingerprint, on a repo that did nothing wrong.
func TestPackPartitionCapsMatchTheParser(t *testing.T) {
	root := repoRoot(t)
	src, err := os.ReadFile(filepath.Join(root, "pack", "internal", "protocol", "partition.go"))
	if err != nil {
		t.Fatalf("read the pack's partition source: %v", err)
	}
	text := string(src)

	for _, c := range []struct {
		name string
		decl string
		want int
	}{
		{"MaxPartitionListBytes", "MaxPartitionListBytes   = 256 * 1024", task.MaxPartitionListBytes},
		{"MaxPartitionObjectBytes", "MaxPartitionObjectBytes = 2048", task.MaxPartitionObjectBytes},
		{"MaxPartitionAttributes", "MaxPartitionAttributes  = 16", task.MaxPartitionAttributes},
		{"MaxPartitionKeyBytes", "MaxPartitionKeyBytes    = 256", task.MaxPartitionKeyBytes},
	} {
		if !strings.Contains(text, c.decl) {
			t.Fatalf("pack/internal/protocol/partition.go no longer declares %q; "+
				"if the cap moved, it must move to match pkg/task's %s (%d)", c.decl, c.name, c.want)
		}
	}

	if task.MaxPartitionListBytes != 256*1024 ||
		task.MaxPartitionObjectBytes != 2048 ||
		task.MaxPartitionAttributes != 16 ||
		task.MaxPartitionKeyBytes != 256 {
		t.Fatalf("pkg/task's partition caps changed; update pack/internal/protocol/partition.go to match "+
			"(list=%d object=%d attrs=%d key=%d)",
			task.MaxPartitionListBytes, task.MaxPartitionObjectBytes,
			task.MaxPartitionAttributes, task.MaxPartitionKeyBytes)
	}
}

func outputKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
