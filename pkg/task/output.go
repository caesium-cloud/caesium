package task

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const (
	// outputMarker is the stdout line prefix that tasks use to emit structured
	// output key-value pairs.  Example:
	//
	//   ##caesium::output {"row_count": "42", "path": "/data/out.parquet"}
	outputMarker = "##caesium::output "

	// branchMarker is the stdout line prefix that branch-type tasks use to
	// indicate which downstream steps should execute.  Example:
	//
	//   ##caesium::branch full-refresh
	branchMarker = "##caesium::branch "

	// MaxOutputBytes caps the total serialised size of collected outputs per
	// task to prevent unbounded memory/DB usage.  Tasks that need to pass
	// larger payloads should use shared storage and pass the reference.
	MaxOutputBytes = 65536 // 64 KB

	// MaxLogSnapshotBytes caps the amount of raw task log text that Caesium
	// persists for completed tasks. This gives the UI a durable snapshot to
	// search and review after the runtime itself has been cleaned up.
	MaxLogSnapshotBytes = 1 << 20 // 1 MiB
)

// ParseOutput reads container log output and extracts structured key-value
// pairs from lines matching the ##caesium::output marker protocol.
//
// Multiple marker lines are merged with last-write-wins semantics per key.
// All values are coerced to strings.  If the total serialised output exceeds
// MaxOutputBytes an error is returned.
//
// Lines that do not match the marker prefix are silently ignored (they are
// normal log output).
func ParseOutput(logs io.Reader) (map[string]string, error) {
	result := make(map[string]string)

	scanner := bufio.NewScanner(logs)
	for scanner.Scan() {
		line := scanner.Text()

		// Docker multiplexed log lines may have an 8-byte binary header.
		// The marker will still appear in the text portion of the line.
		idx := strings.Index(line, outputMarker)
		if idx < 0 {
			continue
		}

		payload := line[idx+len(outputMarker):]
		payload = strings.TrimSpace(payload)
		if payload == "" {
			continue
		}

		var raw map[string]any
		if err := json.Unmarshal([]byte(payload), &raw); err != nil {
			// Malformed JSON on an output line — skip rather than failing
			// the entire task.
			continue
		}

		for k, v := range raw {
			result[k] = fmt.Sprintf("%v", v)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading task output logs: %w", err)
	}

	if len(result) == 0 {
		return nil, nil
	}

	// Enforce size limit.
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshalling task output: %w", err)
	}
	if len(encoded) > MaxOutputBytes {
		return nil, fmt.Errorf("task output exceeds %d byte limit (%d bytes)", MaxOutputBytes, len(encoded))
	}

	return result, nil
}

// ParseBranches reads container log output and extracts branch selection
// markers.  Each line matching "##caesium::branch <step-name>" adds the
// step name to the returned slice.  Duplicate names are deduplicated while
// preserving first-seen order.
//
// Lines that do not match the marker prefix are silently ignored.
func ParseBranches(logs io.Reader) ([]string, error) {
	var result []string
	seen := make(map[string]struct{})

	scanner := bufio.NewScanner(logs)
	for scanner.Scan() {
		line := scanner.Text()

		idx := strings.Index(line, branchMarker)
		if idx < 0 {
			continue
		}

		name := strings.TrimSpace(line[idx+len(branchMarker):])
		if name == "" {
			continue
		}

		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading branch markers: %w", err)
	}

	return result, nil
}

// Markers holds the results of a unified single-pass parse of container logs,
// extracting both structured output key-value pairs and branch selection markers
// without buffering the entire log stream in memory.
type Markers struct {
	Output       map[string]string
	Branches     []string
	LogText      string
	LogTruncated bool
}

// ParseMarkers reads container log output in a single pass and extracts both
// structured output (##caesium::output) and branch selection (##caesium::branch)
// markers.  This is more memory-efficient than calling ParseOutput and
// ParseBranches separately, as it avoids buffering the entire log stream.
func ParseMarkers(logs io.Reader) (*Markers, error) {
	return parseMarkers(logs, nil)
}

// CaptureMarkers reads container log output in a single pass, extracting
// structured markers while also capturing a bounded raw log snapshot suitable
// for UI display after the runtime has been cleaned up.
func CaptureMarkers(logs io.Reader, maxSnapshotBytes int) (*Markers, error) {
	if maxSnapshotBytes <= 0 {
		return parseMarkers(logs, nil)
	}

	snapshot := &boundedSnapshotWriter{limit: maxSnapshotBytes}
	result, err := parseMarkers(logs, snapshot)
	if err != nil {
		return nil, err
	}
	if result != nil {
		result.LogText = snapshot.String()
		result.LogTruncated = snapshot.truncated
	}
	return result, nil
}

func parseMarkers(logs io.Reader, snapshot io.Writer) (*Markers, error) {
	output := make(map[string]string)
	var branches []string
	branchSeen := make(map[string]struct{})

	reader := logs
	if snapshot != nil {
		reader = io.TeeReader(logs, snapshot)
	}

	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()

		// Check for output marker.
		if idx := strings.Index(line, outputMarker); idx >= 0 {
			payload := strings.TrimSpace(line[idx+len(outputMarker):])
			if payload != "" {
				var raw map[string]any
				if err := json.Unmarshal([]byte(payload), &raw); err == nil {
					for k, v := range raw {
						output[k] = fmt.Sprintf("%v", v)
					}
				}
			}
		}

		// Check for branch marker (same line could theoretically match both,
		// but in practice markers are distinct).
		if idx := strings.Index(line, branchMarker); idx >= 0 {
			name := strings.TrimSpace(line[idx+len(branchMarker):])
			if name != "" {
				if _, ok := branchSeen[name]; !ok {
					branchSeen[name] = struct{}{}
					branches = append(branches, name)
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading task log markers: %w", err)
	}

	result := &Markers{}

	if len(output) > 0 {
		// Enforce size limit.
		encoded, err := json.Marshal(output)
		if err != nil {
			return nil, fmt.Errorf("marshalling task output: %w", err)
		}
		if len(encoded) > MaxOutputBytes {
			return nil, fmt.Errorf("task output exceeds %d byte limit (%d bytes)", MaxOutputBytes, len(encoded))
		}
		result.Output = output
	}

	result.Branches = branches
	return result, nil
}

type boundedSnapshotWriter struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (w *boundedSnapshotWriter) Write(p []byte) (int, error) {
	if w.limit <= 0 {
		w.truncated = len(p) > 0
		return len(p), nil
	}

	remaining := w.limit - w.buf.Len()
	if remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		if _, err := w.buf.Write(p[:remaining]); err != nil {
			return 0, err
		}
	}

	if len(p) > remaining {
		w.truncated = true
	}

	return len(p), nil
}

func (w *boundedSnapshotWriter) String() string {
	return w.buf.String()
}

// NormalizeStepName converts a step name to an environment-variable-safe
// prefix.  Hyphens and dots are replaced with underscores and the result is
// uppercased.
//
//	"etl-extract" → "ETL_EXTRACT"
//	"step.one"    → "STEP_ONE"
func NormalizeStepName(name string) string {
	name = strings.ReplaceAll(name, "-", "_")
	name = strings.ReplaceAll(name, ".", "_")
	return strings.ToUpper(name)
}

// BuildOutputEnv constructs CAESIUM_OUTPUT_<STEP>_<KEY>=<VALUE> environment
// variables from a map of predecessor step names to their output key-value
// pairs.
func BuildOutputEnv(predecessorOutputs map[string]map[string]string) map[string]string {
	if len(predecessorOutputs) == 0 {
		return nil
	}

	env := make(map[string]string)
	for stepName, outputs := range predecessorOutputs {
		prefix := "CAESIUM_OUTPUT_" + NormalizeStepName(stepName) + "_"
		for k, v := range outputs {
			envKey := prefix + NormalizeStepName(k)
			env[envKey] = v
		}
	}

	if len(env) == 0 {
		return nil
	}
	return env
}
