package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// DigestPrefix is the algorithm label Caesium requires on every digest it folds
// into a cache identity hash.
const DigestPrefix = "sha256:"

// outputRefPayload is the wire shape of an ##caesium::output-ref line. It
// mirrors pkg/task's parser: key, path, digest and size, with the digest
// validated as "sha256:" + 64 lowercase hex characters. A malformed reference
// is silently dropped by the server, which would look to a role like a
// successful emission, so the emitter validates before staging.
type outputRefPayload struct {
	Key    string `json:"key"`
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// OutputRef stages an ##caesium::output-ref marker for the file at path. The
// digest and size are computed here, by streaming the file, so a role never
// reports a digest it did not actually measure — the digest is what makes a
// downstream cache hit value-verified rather than path-based.
//
// path is emitted verbatim: it is a path inside the role's own mounts, which is
// what the consuming container will see.
func (e *Emitter) OutputRef(key, path string) error {
	if err := validScalar("output-ref key", key); err != nil {
		return err
	}
	if err := validScalar("output-ref path", path); err != nil {
		return err
	}
	f, err := os.Open(path) //nolint:gosec // the path is the role's own artifact.
	if err != nil {
		return fmt.Errorf("output-ref %q: open %s: %w", key, path, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("output-ref %q: stat %s: %w", key, path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("output-ref %q: %s is a directory", key, path)
	}

	sum := sha256.New()
	size, err := io.Copy(sum, f)
	if err != nil {
		return fmt.Errorf("output-ref %q: read %s: %w", key, path, err)
	}
	return e.OutputRefDigest(key, path, DigestPrefix+hex.EncodeToString(sum.Sum(nil)), size)
}

// OutputRefDigest stages a reference whose digest was measured elsewhere (for
// example by a tool that already hashed its own artifact). digest must carry
// the sha256: prefix; size must be non-negative.
func (e *Emitter) OutputRefDigest(key, path, digest string, size int64) error {
	if err := validScalar("output-ref key", key); err != nil {
		return err
	}
	if err := validScalar("output-ref path", path); err != nil {
		return err
	}
	if !ValidDigest(digest) {
		return fmt.Errorf("output-ref %q: %q is not a sha256:<64 hex> digest", key, digest)
	}
	if size < 0 {
		return fmt.Errorf("output-ref %q: negative size %d", key, size)
	}
	payload, err := json.Marshal(outputRefPayload{Key: key, Path: path, Digest: digest, Size: size})
	if err != nil {
		return fmt.Errorf("output-ref %q: marshal: %w", key, err)
	}
	e.stage(OutputRefMarker + " " + string(payload))
	return nil
}

// ValidDigest reports whether s is "sha256:" followed by 64 lowercase hex
// characters — the only form Caesium accepts in a reference or a partition
// fingerprint.
func ValidDigest(s string) bool {
	if len(s) != len(DigestPrefix)+64 {
		return false
	}
	if s[:len(DigestPrefix)] != DigestPrefix {
		return false
	}
	for i := len(DigestPrefix); i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// Digest renders raw bytes as a Caesium digest string.
func Digest(sum []byte) string { return DigestPrefix + hex.EncodeToString(sum) }
