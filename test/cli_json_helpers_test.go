//go:build integration

package test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// H-2: `--json` stdout discipline. Every scenario that asserts on a CLI verb's
// machine-readable output goes through these helpers: stdout is captured
// separately from stderr (runCLIStdout / runCLISeparate, never the merged
// runCLIRaw), and any byte on stdout that is not part of exactly one JSON
// document fails the scenario. Leading and trailing whitespace is the only
// slack. A log line or cobra Print* call that leaks onto stdout therefore
// fails here even when the JSON after it would still decode.

// decodeCLIJSONStdout decodes stdout as exactly one JSON document into out.
func decodeCLIJSONStdout(stdout []byte, out any) error {
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 {
		return errors.New("stdout is empty; expected one JSON document")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("stdout is not a JSON document: %w\nstdout: %q", err, stdout)
	}
	if rest := trimmed[dec.InputOffset():]; len(bytes.TrimSpace(rest)) != 0 {
		return fmt.Errorf("stdout has %d non-JSON byte(s) after the document: %q", len(rest), rest)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode stdout JSON into %T: %w", out, err)
	}
	return nil
}

// runCLIJSON runs a CLI verb that must succeed and returns its stdout decoded
// as exactly one JSON document.
func runCLIJSON[T any](s *IntegrationTestSuite, args ...string) T {
	s.T().Helper()
	stdout, err := s.runCLIStdout(args...)
	s.Require().NoError(err, "caesium %v failed; stdout: %s", args, stdout)
	var out T
	s.Require().NoError(decodeCLIJSONStdout([]byte(stdout), &out), "caesium %v", args)
	return out
}

// runCLIJSONFailure runs a CLI verb that must exit non-zero yet still print
// exactly one JSON document on stdout (a rejected `--json` lint or reload).
// It returns the decoded document and stderr.
func runCLIJSONFailure[T any](s *IntegrationTestSuite, args ...string) (T, string) {
	s.T().Helper()
	stdout, stderr, err := s.runCLISeparate(args...)
	s.Require().Error(err, "caesium %v must exit non-zero; stdout: %s", args, stdout)
	var out T
	s.Require().NoError(decodeCLIJSONStdout([]byte(stdout), &out), "caesium %v (stderr: %s)", args, stderr)
	return out, stderr
}

// TestDecodeCLIJSONStdoutRejectsContamination pins the helper itself, so a
// permissive decoder cannot make every --json scenario hollow.
func TestDecodeCLIJSONStdoutRejectsContamination(t *testing.T) {
	for _, stdout := range []string{
		"\n  {\"ok\": true}\n\n",
		"[1, 2]\n",
	} {
		var out any
		if err := decodeCLIJSONStdout([]byte(stdout), &out); err != nil {
			t.Errorf("%q: unexpected error %v", stdout, err)
		}
	}
	for _, stdout := range []string{
		"",
		"  \n",
		"Validated 1 file\n{\"ok\": true}\n",
		"{\"ok\": true}\nwarning: trailing log line\n",
		"{\"ok\": true}{\"ok\": false}\n",
		"{\"level\":\"info\",\"msg\":\"log\"}\n{\"ok\": true}\n",
		"{\"ok\": tru",
	} {
		var out any
		if err := decodeCLIJSONStdout([]byte(stdout), &out); err == nil {
			t.Errorf("%q: accepted contaminated stdout", stdout)
		}
	}
}
