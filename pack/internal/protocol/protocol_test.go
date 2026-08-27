package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestEmitter() (*Emitter, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	return New("test-role", &out, &errOut), &out, &errOut
}

func TestOutputMarkerIsSortedAndSingleLine(t *testing.T) {
	e, out, _ := newTestEmitter()
	if err := e.Output(map[string]string{"treeDigest": "sha256:ab", "commit": "deadbeef", "path": "/src"}); err != nil {
		t.Fatalf("Output: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("marker written before Flush: %q", out.String())
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	got := strings.TrimRight(out.String(), "\n")
	if strings.Contains(got, "\n") {
		t.Fatalf("marker spans multiple lines: %q", got)
	}
	want := `##caesium::output {"commit":"deadbeef","path":"/src","treeDigest":"sha256:ab"}`
	if got != want {
		t.Fatalf("marker = %q, want %q", got, want)
	}
}

func TestOutputRejectsBadKeysAndValues(t *testing.T) {
	cases := map[string]map[string]string{
		"empty map":       {},
		"empty key":       {"": "v"},
		"newline in key":  {"a\nb": "v"},
		"newline in val":  {"k": "line1\nline2"},
		"invalid utf8":    {"k": string([]byte{0xff, 0xfe})},
		"blank-space key": {"   ": "v"},
	}
	for name, values := range cases {
		t.Run(name, func(t *testing.T) {
			e, out, _ := newTestEmitter()
			if err := e.Output(values); err == nil {
				t.Fatalf("Output(%v) = nil error, want rejection", values)
			}
			if err := e.Flush(); err != nil {
				t.Fatalf("Flush: %v", err)
			}
			if out.Len() != 0 {
				t.Fatalf("rejected output still wrote %q", out.String())
			}
		})
	}
}

func TestOutputRejectsOversizePayload(t *testing.T) {
	e, _, _ := newTestEmitter()
	err := e.Output(map[string]string{"blob": strings.Repeat("x", MaxOutputBytes+1)})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Output(oversize) = %v, want a size rejection", err)
	}
}

func TestFailClosedWritesNoMarker(t *testing.T) {
	e, out, errOut := newTestEmitter()
	if err := e.Output(map[string]string{"fingerprint": "sha256:" + strings.Repeat("a", 64)}); err != nil {
		t.Fatalf("Output: %v", err)
	}
	if len(e.Buffered()) != 1 {
		t.Fatalf("Buffered() = %v, want one staged marker", e.Buffered())
	}

	code := e.FailClosed(errors.New("terraform get failed"))
	if code == 0 {
		t.Fatal("FailClosed returned 0; a failed role must exit non-zero")
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush after FailClosed: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("FailClosed left %q on stdout; the buffer must be discarded", out.String())
	}
	if !strings.Contains(errOut.String(), "terraform get failed") {
		t.Fatalf("stderr = %q, want the cause", errOut.String())
	}
	if !strings.Contains(errOut.String(), "test-role") {
		t.Fatalf("stderr = %q, want the role name", errOut.String())
	}
}

func TestFailClosedWithNilErrorStillFails(t *testing.T) {
	e, _, errOut := newTestEmitter()
	if code := e.FailClosed(nil); code == 0 {
		t.Fatal("FailClosed(nil) = 0, want non-zero")
	}
	if errOut.Len() == 0 {
		t.Fatal("FailClosed(nil) wrote nothing to stderr")
	}
}

func TestFlushIsIdempotent(t *testing.T) {
	e, out, _ := newTestEmitter()
	if err := e.Branch("full-refresh"); err != nil {
		t.Fatalf("Branch: %v", err)
	}
	for range 3 {
		if err := e.Flush(); err != nil {
			t.Fatalf("Flush: %v", err)
		}
	}
	if got := strings.Count(out.String(), BranchMarker); got != 1 {
		t.Fatalf("branch marker written %d times, want 1", got)
	}
}

func TestBranchRejectsEmptyTargets(t *testing.T) {
	e, _, _ := newTestEmitter()
	if err := e.Branch(); err == nil {
		t.Fatal("Branch() with no targets = nil error")
	}
	if err := e.Branch("ok", " "); err == nil {
		t.Fatal("Branch with a blank target = nil error")
	}
}

func TestOutputRefHashesTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tf.plan")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	e, out, _ := newTestEmitter()
	if err := e.OutputRef("proposal_artifact", path); err != nil {
		t.Fatalf("OutputRef: %v", err)
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	line := strings.TrimRight(out.String(), "\n")
	payload, ok := strings.CutPrefix(line, OutputRefMarker+" ")
	if !ok {
		t.Fatalf("line %q does not carry the output-ref marker", line)
	}
	var ref outputRefPayload
	if err := json.Unmarshal([]byte(payload), &ref); err != nil {
		t.Fatalf("unmarshal %q: %v", payload, err)
	}
	// sha256("hello")
	const want = "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if ref.Digest != want {
		t.Fatalf("digest = %q, want %q", ref.Digest, want)
	}
	if ref.Size != 5 {
		t.Fatalf("size = %d, want 5", ref.Size)
	}
	if ref.Path != path {
		t.Fatalf("path = %q, want %q", ref.Path, path)
	}
	if ref.Key != "proposal_artifact" {
		t.Fatalf("key = %q", ref.Key)
	}
}

func TestOutputRefFailsClosedOnMissingFile(t *testing.T) {
	e, out, _ := newTestEmitter()
	if err := e.OutputRef("artifact", filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("OutputRef on a missing file = nil error")
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("failed OutputRef still emitted %q", out.String())
	}
}

func TestOutputRefRejectsDirectory(t *testing.T) {
	e, _, _ := newTestEmitter()
	if err := e.OutputRef("artifact", t.TempDir()); err == nil {
		t.Fatal("OutputRef on a directory = nil error")
	}
}

func TestOutputRefDigestValidatesDigestAndSize(t *testing.T) {
	good := "sha256:" + strings.Repeat("a", 64)
	cases := []struct {
		name   string
		digest string
		size   int64
	}{
		{"no prefix", strings.Repeat("a", 64), 1},
		{"short hex", "sha256:abc", 1},
		{"uppercase hex", "sha256:" + strings.Repeat("A", 64), 1},
		{"negative size", good, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _, _ := newTestEmitter()
			if err := e.OutputRefDigest("k", "/p", tc.digest, tc.size); err == nil {
				t.Fatalf("OutputRefDigest(%q, %d) = nil error", tc.digest, tc.size)
			}
		})
	}
	e, _, _ := newTestEmitter()
	if err := e.OutputRefDigest("k", "/p", good, 0); err != nil {
		t.Fatalf("OutputRefDigest(valid) = %v", err)
	}
}

func TestValidDigest(t *testing.T) {
	if !ValidDigest("sha256:" + strings.Repeat("0", 64)) {
		t.Fatal("valid digest rejected")
	}
	for _, bad := range []string{"", "sha256:", "sha512:" + strings.Repeat("0", 64), "sha256:" + strings.Repeat("g", 64)} {
		if ValidDigest(bad) {
			t.Fatalf("ValidDigest(%q) = true", bad)
		}
	}
}
