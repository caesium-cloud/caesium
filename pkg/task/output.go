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

	// outputRefMarker is the stdout line prefix that tasks use to emit a
	// reference to a large payload offloaded to a BYO volume/object store.
	// Instead of inlining the payload (which is capped at MaxOutputBytes), the
	// step writes it to a mounted volume and emits only a content-addressed
	// reference — the path it wrote and the SHA-256 digest of the bytes:
	//
	//   ##caesium::output-ref {"key":"frame","path":"/data/out.parquet","digest":"sha256:ab…","size":734003200}
	//
	// Caesium keeps only the bounded reference (never the blob) in dqlite,
	// folds the digest into the task-identity hash so a byte-identical payload
	// is a cache hit, and passes the path + digest to downstream containers via
	// CAESIUM_OUTPUT_<STEP>_<KEY> / _DIGEST. This is the substrate for
	// value-verified skip (large-object reference passing, design Component 5).
	outputRefMarker = "##caesium::output-ref "

	// branchMarker is the stdout line prefix that branch-type tasks use to
	// indicate which downstream steps should execute.  Example:
	//
	//   ##caesium::branch full-refresh
	branchMarker = "##caesium::branch "

	// MaxOutputBytes caps the total serialised size of collected outputs per
	// task to prevent unbounded memory/DB usage.  Tasks that need to pass
	// larger payloads should use shared storage and pass the reference via the
	// ##caesium::output-ref marker (see OutputRef); the reference itself is
	// small and counts against this cap, the payload it points at does not.
	MaxOutputBytes = 65536 // 64 KB

	// MaxLogSnapshotBytes caps the amount of raw task log text that Caesium
	// persists for completed tasks. This gives the UI a durable snapshot to
	// search and review after the runtime itself has been cleaned up.
	MaxLogSnapshotBytes = 1 << 20 // 1 MiB
)

// outputRefVersion is the schema version of the canonical reference value
// encoded into the output map (see OutputRef.Encode). It is independent of the
// cache version: it versions the on-the-wire/in-dqlite reference encoding so a
// reader can tell two encodings apart. Bumping it changes the encoded value and
// therefore the cache key for any step that consumes a reference — treat it the
// way CacheVersion is treated.
const outputRefVersion = 1

// OutputRef is a content-addressed reference to a large payload a step wrote to
// a BYO volume/object store instead of inlining it as a structured output. Only
// the reference (path + digest + size) crosses the step boundary and lands in
// dqlite; the payload stays on the volume. The Digest is the load-bearing field
// for caching: it is folded into the consuming step's identity hash (via the
// encoded value, see Encode), so a step that re-emits a byte-identical payload —
// hence an identical digest — produces a cache hit. A path change with an
// unchanged digest does NOT change the hash, which is the point: the value, not
// its location, decides equality.
type OutputRef struct {
	// Ref is outputRefVersion at emit time. It is the first field so a future
	// reader can dispatch on the encoding version before trusting the rest.
	Ref int `json:"caesiumOutputRef"`
	// Path is the location the producing container wrote the payload to (a path
	// inside a mounted BYO volume). It is informational for downstream
	// containers (exposed as CAESIUM_OUTPUT_<STEP>_<KEY>) and deliberately NOT
	// part of the equality decision — only Digest is.
	Path string `json:"path"`
	// Digest is "sha256:" + the lowercase hex SHA-256 of the payload bytes,
	// computed by the producing container. It is the content address; folding
	// it into the hash is what makes the skip value-verified rather than
	// path/heuristic based.
	Digest string `json:"digest"`
	// Size is the payload size in bytes, if the producer reported it. Advisory
	// only (it does not participate in the digest); 0 means unreported.
	Size int64 `json:"size,omitempty"`
}

// outputRefPayload is the JSON shape a step emits on an ##caesium::output-ref
// line. It is intentionally separate from OutputRef: the wire payload carries
// the destination key and lets the producer omit the version (defaulted on
// parse), whereas OutputRef is the validated, canonical value that gets encoded
// into the output map.
type outputRefPayload struct {
	Key    string `json:"key"`
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// outputRefSentinel is the discriminator key whose presence in an encoded
// output value identifies it as a reference rather than a plain scalar. It is
// the JSON field name of OutputRef.Ref; DecodeOutputRef keys off it so a plain
// value that merely looks like JSON is never mistaken for a reference.
const outputRefSentinel = "caesiumOutputRef"

// Encode renders the reference as the canonical string stored under its key in
// the output map. encoding/json emits the struct fields in declaration order
// deterministically, so two byte-identical payloads (same digest) encode to the
// same string — which is exactly what folds into the consuming step's identity
// hash and yields a cache hit. The path is included for downstream consumers
// but, because the digest is present and the hash already covers the whole
// value, equality still tracks content: a moved file with identical bytes keeps
// the same digest and therefore re-uses the cache.
func (r OutputRef) Encode() string {
	// Marshalling a fixed struct with no maps is deterministic; the error path
	// is unreachable for these field types, but guard it rather than ignore it.
	data, err := json.Marshal(r)
	if err != nil {
		// Fall back to a stable, digest-bearing string so the value still
		// changes with content even in the (unreachable) error case.
		return fmt.Sprintf(`{"%s":%d,"digest":%q}`, outputRefSentinel, r.Ref, r.Digest)
	}
	return string(data)
}

// IsOutputRef reports whether an output-map value is an encoded OutputRef. It is
// a cheap prefix/substring check used by BuildOutputEnv and the lineage mapper
// to treat references differently from scalars without fully decoding every
// value.
func IsOutputRef(value string) bool {
	// A reference is a JSON object whose first key is the sentinel. Require the
	// quoted sentinel key to avoid matching a scalar that merely contains the
	// word.
	return strings.HasPrefix(value, `{"`+outputRefSentinel+`"`)
}

// DecodeOutputRef parses an encoded reference value back into an OutputRef. It
// returns ok=false (not an error) for any value that is not a well-formed
// reference, so callers can treat non-reference values as plain scalars.
func DecodeOutputRef(value string) (OutputRef, bool) {
	if !IsOutputRef(value) {
		return OutputRef{}, false
	}
	var ref OutputRef
	if err := json.Unmarshal([]byte(value), &ref); err != nil {
		return OutputRef{}, false
	}
	if ref.Ref == 0 || ref.Digest == "" {
		return OutputRef{}, false
	}
	return ref, true
}

// parseOutputRefLine parses one ##caesium::output-ref payload into a key and its
// canonical encoded value. It returns ok=false for a malformed or incomplete
// reference (missing key/path/digest, a digest that is not a sha256: hex string,
// a negative reported size, or a size exceeding maxRefBytes when that cap is > 0)
// so a bad line is skipped rather than failing the whole task — the same lenient
// posture ParseOutput takes for malformed ##caesium::output lines.
func parseOutputRefLine(payload string, maxRefBytes int64) (key, encoded string, ok bool) {
	var p outputRefPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return "", "", false
	}
	p.Key = strings.TrimSpace(p.Key)
	p.Path = strings.TrimSpace(p.Path)
	p.Digest = strings.TrimSpace(p.Digest)
	if p.Key == "" || p.Path == "" || !validSHA256Ref(p.Digest) {
		return "", "", false
	}
	// Reject a physically invalid size: a negative value is meaningless and could
	// slip under the maxRefBytes upper bound below (a negative is never > the
	// cap), so it is screened first.
	if p.Size < 0 {
		return "", "", false
	}
	if maxRefBytes > 0 && p.Size > maxRefBytes {
		return "", "", false
	}
	ref := OutputRef{
		Ref:    outputRefVersion,
		Path:   p.Path,
		Digest: p.Digest,
		Size:   p.Size,
	}
	return p.Key, ref.Encode(), true
}

// validSHA256Ref reports whether s is a well-formed "sha256:<64 hex>" digest.
// Enforcing the shape keeps a malformed digest out of the cache key (where it
// would otherwise silently weaken content-addressing).
func validSHA256Ref(s string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(s, prefix) {
		return false
	}
	hex := s[len(prefix):]
	if len(hex) != 64 {
		return false
	}
	for i := 0; i < len(hex); i++ {
		c := hex[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

var initialScannerBuffer = make([]byte, 0, 64*1024)

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

	scanner := newLogScanner(logs)
	for scanner.Scan() {
		line := scanner.Text()

		// Reference markers are checked first: ##caesium::output-ref shares the
		// "##caesium::output" stem with the scalar marker, so a reference line
		// must be claimed here before the scalar branch (whose marker has a
		// trailing space) would otherwise miss it anyway. Docker multiplexed log
		// lines may carry an 8-byte binary header; the marker still appears in
		// the text portion.
		if idx := strings.Index(line, outputRefMarker); idx >= 0 {
			payload := strings.TrimSpace(line[idx+len(outputRefMarker):])
			if key, encoded, ok := parseOutputRefLine(payload, 0); ok {
				result[key] = encoded
			}
			continue
		}

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

	scanner := newLogScanner(logs)
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
	return parseMarkers(logs, nil, 0)
}

// CaptureMarkers reads container log output in a single pass, extracting
// structured markers while also capturing a bounded raw log snapshot suitable
// for UI display after the runtime has been cleaned up.
func CaptureMarkers(logs io.Reader, maxSnapshotBytes int) (*Markers, error) {
	return CaptureMarkersWithRefLimit(logs, maxSnapshotBytes, 0)
}

// CaptureMarkersWithRefLimit is CaptureMarkers plus an operator-configured cap on
// the payload size a large-object reference (##caesium::output-ref) may declare.
// maxRefBytes <= 0 means unbounded. A reference whose reported size exceeds the
// cap is dropped (the producer's other outputs still apply); see
// env.Environment.OutputRefMaxBytes for the rationale.
func CaptureMarkersWithRefLimit(logs io.Reader, maxSnapshotBytes int, maxRefBytes int64) (*Markers, error) {
	if maxSnapshotBytes <= 0 {
		return parseMarkers(logs, nil, maxRefBytes)
	}

	snapshot := &boundedSnapshotWriter{limit: maxSnapshotBytes}
	result, err := parseMarkers(logs, snapshot, maxRefBytes)
	if err != nil {
		return nil, err
	}
	if result != nil {
		result.LogText = snapshot.String()
		result.LogTruncated = snapshot.truncated
	}
	return result, nil
}

func parseMarkers(logs io.Reader, snapshot io.Writer, maxRefBytes int64) (*Markers, error) {
	output := make(map[string]string)
	var branches []string
	branchSeen := make(map[string]struct{})

	reader := logs
	if snapshot != nil {
		reader = io.TeeReader(logs, snapshot)
	}

	scanner := newLogScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()

		// Check for the reference-output marker first. ##caesium::output-ref does
		// not contain the trailing-space "##caesium::output " stem, so the scalar
		// branch below already wouldn't claim it; handling it explicitly keeps the
		// two paths independent and self-documenting.
		if idx := strings.Index(line, outputRefMarker); idx >= 0 {
			payload := strings.TrimSpace(line[idx+len(outputRefMarker):])
			if key, encoded, ok := parseOutputRefLine(payload, maxRefBytes); ok {
				output[key] = encoded
			}
		} else if idx := strings.Index(line, outputMarker); idx >= 0 {
			// Check for the scalar output marker (only when the line was not a
			// reference; the two markers are mutually exclusive per line).
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
	if len(p) == 0 {
		return 0, nil
	}

	if w.limit <= 0 {
		w.truncated = true
		return len(p), nil
	}

	if w.buf.Len() >= w.limit {
		w.truncated = true
		return len(p), nil
	}

	spaceLeft := w.limit - w.buf.Len()
	bytesToWrite := p
	if len(bytesToWrite) > spaceLeft {
		bytesToWrite = p[:spaceLeft]
		w.truncated = true
	}

	if _, err := w.buf.Write(bytesToWrite); err != nil {
		return 0, err
	}

	return len(p), nil
}

func (w *boundedSnapshotWriter) String() string {
	return w.buf.String()
}

func newLogScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(initialScannerBuffer, MaxLogSnapshotBytes)
	return scanner
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
//
// Scalar outputs map to a single CAESIUM_OUTPUT_<STEP>_<KEY> var carrying the
// value verbatim. A reference output (an encoded OutputRef) instead exposes the
// volume path the downstream container should read — CAESIUM_OUTPUT_<STEP>_<KEY>
// is set to the path, not the raw JSON — plus a companion
// CAESIUM_OUTPUT_<STEP>_<KEY>_DIGEST carrying the content digest so a consumer
// can re-verify the bytes it reads. The container contract stays string-only
// env vars: a large payload never enters the environment, only its location and
// digest do.
func BuildOutputEnv(predecessorOutputs map[string]map[string]string) map[string]string {
	if len(predecessorOutputs) == 0 {
		return nil
	}

	env := make(map[string]string)
	for stepName, outputs := range predecessorOutputs {
		prefix := "CAESIUM_OUTPUT_" + NormalizeStepName(stepName) + "_"
		for k, v := range outputs {
			envKey := prefix + NormalizeStepName(k)
			if ref, ok := DecodeOutputRef(v); ok {
				// Reference: hand the consumer the path to read and the digest
				// to verify against, never the encoded JSON.
				env[envKey] = ref.Path
				env[envKey+"_DIGEST"] = ref.Digest
				continue
			}
			env[envKey] = v
		}
	}

	if len(env) == 0 {
		return nil
	}
	return env
}
