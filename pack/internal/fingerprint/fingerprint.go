// Package fingerprint digests the inputs that decide a unit's result.
//
// Determinism is the contract (design §6.2): a fingerprint that differs between
// two workers over the same tree splits the cache and silently re-applies
// everything. So: relative paths only, sorted traversal in byte order, file
// contents rather than mtimes, and no locale-dependent comparison anywhere.
package fingerprint

import (
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DigestPrefix is the algorithm label on every digest this package returns.
const DigestPrefix = "sha256:"

// ConfigGlobs are the file patterns that define a Terraform module's behaviour
// (design §6.2). Matching is non-recursive: every nested module is its own
// manifest entry with its own directory, so recursing would double-count the
// children and still miss nothing.
//
// *.tf.json is included beyond the design's list because Terraform treats JSON
// configuration exactly like HCL; leaving it out would let a JSON-configured
// stack change without moving its fingerprint.
var ConfigGlobs = []string{
	"*.tf",
	"*.tf.json",
	"*.tfvars",
	"*.tfvars.json",
	"*.tfquery.hcl",
	".terraform.lock.hcl",
}

// Input is one named contributor to a fingerprint. Emitting these alongside the
// fingerprint is what lets `caesium why` name which input moved rather than
// only that something did.
type Input struct {
	// Name is the stable label, e.g. "root" or a module key.
	Name string
	// Path is the directory as it appears in the module manifest — relative to
	// the root module, never absolute.
	Path string
	// Digest is DigestPrefix + hex.
	Digest string
}

// DigestDir hashes the configuration files directly inside dir.
//
// The digest covers each file's name and its content, so a rename moves it as
// surely as an edit. Files are visited in byte order, never in readdir order.
func DigestDir(dir string) (string, error) {
	names, err := configFiles(dir)
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		// A module directory with no configuration file at all means the
		// manifest pointed somewhere unexpected. Digesting "nothing" would make
		// that look like a stable, unchanging input forever.
		return "", fmt.Errorf("no Terraform configuration files in %s", dir)
	}

	sum := sha256.New()
	for _, name := range names {
		writeLine(sum, name)
		fileSum, err := digestFile(filepath.Join(dir, name))
		if err != nil {
			return "", err
		}
		writeLine(sum, fileSum)
	}
	return DigestPrefix + hexOf(sum.Sum(nil)), nil
}

// Combine folds a sorted set of inputs, plus any extra scalar facts (the
// workspace name), into one fingerprint.
func Combine(inputs []Input, extras ...string) string {
	sorted := append([]Input(nil), inputs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	sum := sha256.New()
	for _, extra := range extras {
		writeLine(sum, "extra")
		writeLine(sum, extra)
	}
	for _, in := range sorted {
		writeLine(sum, "input")
		writeLine(sum, in.Name)
		writeLine(sum, filepath.ToSlash(filepath.Clean(in.Path)))
		writeLine(sum, in.Digest)
	}
	return DigestPrefix + hexOf(sum.Sum(nil))
}

// configFiles lists the configuration file names directly inside dir, sorted in
// byte order and de-duplicated (a name can match more than one glob).
func configFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read module directory %s: %w", dir, err)
	}
	seen := make(map[string]struct{}, len(entries))
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if _, dup := seen[name]; dup {
			continue
		}
		for _, glob := range ConfigGlobs {
			ok, err := filepath.Match(glob, name)
			if err != nil {
				return nil, fmt.Errorf("bad config glob %q: %w", glob, err)
			}
			if ok {
				seen[name] = struct{}{}
				names = append(names, name)
				break
			}
		}
	}
	// sort.Strings is a byte comparison; no locale collation can reorder it on
	// a different runner.
	sort.Strings(names)
	return names, nil
}

func digestFile(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // path built from a directory listing.
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return hexOf(sum.Sum(nil)), nil
}

// writeLine feeds one newline-terminated field to a digest. hash.Hash never
// returns a write error, but discarding it explicitly keeps the intent visible.
func writeLine(h hash.Hash, s string) {
	_, _ = io.WriteString(h, s)
	_, _ = io.WriteString(h, "\n")
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	var out strings.Builder
	out.Grow(len(b) * 2)
	for _, c := range b {
		out.WriteByte(digits[c>>4])
		out.WriteByte(digits[c&0x0f])
	}
	return out.String()
}
