// Package protocol emits the Caesium stdout marker contract that every reagent
// image speaks (design §5.2). It is the only coupling between the reagents and
// Caesium: no SDK, no HTTP client, no import of the Caesium module — a role
// image is a drop-in for any other image that writes the same lines.
//
// Two properties matter more than convenience:
//
//  1. Emission is buffered. Markers are held until Flush, so a role that fails
//     halfway through never leaves a half-truth on stdout that the server would
//     read as a complete result.
//  2. Every role fails closed. FailClosed discards the buffer, so an error
//     after a marker was staged exits non-zero having written no marker at all.
//     An absent fingerprint must never be read as "unchanged" (spec §5.2, §8).
package protocol

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	// OutputMarker prefixes a JSON object of scalar key/value pairs that
	// Caesium stores as the task's structured output and exposes downstream as
	// CAESIUM_OUTPUT_<STEP>_<KEY>.
	OutputMarker = "##caesium::output"

	// OutputRefMarker prefixes a content-addressed reference to a payload the
	// role wrote to a mounted volume instead of inlining it.
	OutputRefMarker = "##caesium::output-ref"

	// PartitionsMarker prefixes a JSON array of partition elements. The reagents
	// always emit the object form ({key, fingerprint, dependsOn, …}) because a
	// bare string carries neither a per-unit fingerprint nor inter-unit order
	// (spec §5.4).
	PartitionsMarker = "##caesium::partitions"

	// BranchMarker prefixes one branch target name. Caesium runs only the named
	// successors of a branch step.
	BranchMarker = "##caesium::branch"
)

// MaxOutputBytes mirrors Caesium's per-task structured-output cap
// (pkg/task.MaxOutputBytes). Emitting more is a producer bug, so the emitter
// rejects it here rather than letting the server silently fail the task.
const MaxOutputBytes = 65536

// Emitter buffers marker lines and writes them only when Flush succeeds.
//
// It is not safe for concurrent use; every reagent role is single-goroutine by
// construction.
type Emitter struct {
	name    string
	out     io.Writer
	errOut  io.Writer
	lines   []string
	flushed bool
}

// New returns an Emitter that will write buffered markers to out and failure
// diagnostics to errOut. name identifies the role in diagnostics.
func New(name string, out, errOut io.Writer) *Emitter {
	return &Emitter{name: name, out: out, errOut: errOut}
}

// Buffered returns a copy of the marker lines staged so far. It exists for
// tests and for roles that want to log what they are about to emit.
func (e *Emitter) Buffered() []string {
	return append([]string(nil), e.lines...)
}

// Discard drops every staged marker. After Discard, Flush writes nothing.
func (e *Emitter) Discard() { e.lines = nil }

// Flush writes every staged marker, in emission order, and clears the buffer.
// A second Flush is a no-op, so a role may call it explicitly and still be
// wrapped by Run.
func (e *Emitter) Flush() error {
	if e.flushed {
		return nil
	}
	e.flushed = true
	for _, line := range e.lines {
		if _, err := fmt.Fprintln(e.out, line); err != nil {
			return fmt.Errorf("write marker: %w", err)
		}
	}
	e.lines = nil
	return nil
}

// FailClosed abandons every staged marker, reports err on the error stream, and
// returns the process exit code to use. It never returns 0: a role that fails
// must not be distinguishable, by its stdout, from a role that never ran.
func (e *Emitter) FailClosed(err error) int {
	e.Discard()
	e.flushed = true
	if err == nil {
		err = fmt.Errorf("failed with no error recorded")
	}
	_, _ = fmt.Fprintf(e.errOut, "%s: %v\n", e.name, err)
	return 1
}

// Output stages an ##caesium::output marker carrying values. Keys and values
// must be non-empty, valid UTF-8 and free of newlines (a newline would split
// the marker across log lines and truncate the JSON).
func (e *Emitter) Output(values map[string]string) error {
	if len(values) == 0 {
		return fmt.Errorf("output: no values")
	}
	for k, v := range values {
		if err := validScalar("output key", k); err != nil {
			return err
		}
		if err := validValue(fmt.Sprintf("output %q", k), v); err != nil {
			return err
		}
	}
	// encoding/json sorts map keys, so the marker is byte-stable for a given
	// map — which matters because these values feed cache identity hashes.
	payload, err := json.Marshal(values)
	if err != nil {
		return fmt.Errorf("output: marshal: %w", err)
	}
	if len(payload) > MaxOutputBytes {
		return fmt.Errorf("output: %d bytes exceeds the %d byte limit", len(payload), MaxOutputBytes)
	}
	e.stage(OutputMarker + " " + string(payload))
	return nil
}

// Branch stages one ##caesium::branch marker per named successor.
func (e *Emitter) Branch(names ...string) error {
	if len(names) == 0 {
		return fmt.Errorf("branch: no targets")
	}
	for _, name := range names {
		if err := validScalar("branch target", name); err != nil {
			return err
		}
		e.stage(BranchMarker + " " + name)
	}
	return nil
}

func (e *Emitter) stage(line string) { e.lines = append(e.lines, line) }

// Run is the process entrypoint every reagent role uses. It builds an Emitter over
// stdout/stderr, invokes fn, and terminates: on success the staged markers are
// flushed and the process exits 0; on any error nothing is written to stdout
// and the process exits non-zero.
func Run(name string, fn func(*Emitter) error) {
	e := New(name, os.Stdout, os.Stderr)
	if err := fn(e); err != nil {
		os.Exit(e.FailClosed(err))
	}
	if err := e.Flush(); err != nil {
		// The buffer is already partially on stdout at this point, but a failed
		// write is an I/O failure, not a contract violation: exit non-zero so
		// the task is red and nothing downstream consumes a truncated marker.
		_, _ = fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		os.Exit(1)
	}
}

// SortedKeys returns m's keys in byte order. Roles use it wherever traversal
// order would otherwise leak into a digest (spec §6.2 determinism requirement).
func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func validScalar(what, s string) error {
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("%s is empty", what)
	}
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s is not valid UTF-8", what)
	}
	if strings.ContainsAny(s, "\r\n") {
		return fmt.Errorf("%s contains a newline", what)
	}
	return nil
}

// validValue is validScalar without the non-empty requirement: an empty output
// value is meaningful ("this key resolved to nothing"), an empty key is not.
func validValue(what, s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s is not valid UTF-8", what)
	}
	if strings.ContainsAny(s, "\r\n") {
		return fmt.Errorf("%s contains a newline", what)
	}
	return nil
}
