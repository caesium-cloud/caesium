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
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DigestPrefix is the algorithm label on every digest this package returns.
const DigestPrefix = "sha256:"

// ConfigGlobs are the file patterns that make a directory a Terraform module
// (design §6.2). They are the PRESENCE test, not the coverage rule: a directory
// with none of them is not a module, and digesting "nothing" there would make a
// mis-resolved manifest entry look like a stable, unchanging input forever.
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

// excludedDirs are directory names DigestDir never descends into, at any depth.
//
// Each holds data Terraform or Caesium generates rather than data the module
// owns: .terraform is the working directory init writes (providers, module
// installs, the manifest), .caesium is the default ARTIFACT_DIR (saved plans,
// apply receipts), and .git is the checkout's own metadata, which moves on
// every commit whether or not the module did.
//
// Everything else under a module directory IS the module: digesting it is what
// keeps an edit to a template, a policy document or a cloud-init script from
// being invisible to the fingerprint.
var excludedDirs = map[string]struct{}{
	".terraform": {},
	".caesium":   {},
	".git":       {},
}

// Input is one named contributor to a fingerprint. Emitting these alongside the
// fingerprint is what lets `caesium why` name which input moved rather than
// only that something did.
type Input struct {
	// Name is the stable label, e.g. "root" or a module key.
	Name string
	// Identity is what distinguishes this input from another with the same
	// content: a path relative to the root module for something in the source
	// tree, or a declared source for something fetched into a scratch
	// directory. It must never carry a machine- or run-specific path — that is
	// the fingerprint-nondeterminism failure this package exists to avoid — and
	// callers are responsible for normalizing it (see tf.Manifest.Resolve).
	Identity string
	// Digest is DigestPrefix + hex.
	Digest string
}

// DigestDir hashes every file a module owns, recursively.
//
// It covers each file's path relative to dir and its content, so a rename moves
// the digest as surely as an edit. Paths are visited in byte order, never in
// readdir order.
//
// Coverage is deliberately by exclusion rather than by an allow-list of
// Terraform file extensions. A module's behaviour is decided by far more than
// its *.tf files: templatefile("${path.module}/templates/userdata.tftpl", …),
// file(), fileset(), an archive_file's source_dir, an IAM policy JSON, a
// cloud-init script. Hashing only the configuration extensions let every one of
// those change without moving the stack's fingerprint — and because plan steps
// key on `chain: values`, the checkout could move while plan and apply cache-hit
// and the run went green having deployed none of the edit. Only generated
// Terraform and state data is left out (see excludedDirs and generatedState),
// because that is what MOVES on its own and would re-apply every stack forever.
//
// excluded names additional absolute paths to skip — the ARTIFACT_DIR when an
// operator has relocated it inside the source tree, where the plan artifacts
// and apply receipts it accumulates would otherwise enter the digest of the
// very stack that writes them.
//
// A nested module directory is digested here AND again as its own manifest
// entry. That double-counting is deliberate: the alternative — skipping
// subdirectories — is what made a nested asset invisible, and counting an edit
// twice moves the fingerprint just as correctly as counting it once.
func DigestDir(dir string, excluded ...string) (string, error) {
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

	files, err := moduleFiles(dir, excluded)
	if err != nil {
		return "", err
	}

	sum := sha256.New()
	for _, file := range files {
		writeLine(sum, file.name)
		writeLine(sum, file.digest)
	}
	return DigestPrefix + hexOf(sum.Sum(nil)), nil
}

// moduleFile is one digested path: its slash-separated location relative to the
// module directory, and what that location contains.
type moduleFile struct {
	name   string
	digest string
}

// moduleFiles walks dir and digests every file the module owns.
func moduleFiles(dir string, excluded []string) ([]moduleFile, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve module directory %s: %w", dir, err)
	}
	skip := make(map[string]struct{}, len(excluded))
	for _, path := range excluded {
		if strings.TrimSpace(path) == "" {
			continue
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve excluded path %s: %w", path, err)
		}
		skip[abs] = struct{}{}
	}

	var files []moduleFile
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if _, dropped := skip[path]; dropped {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if path == root {
				return nil
			}
			if _, dropped := excludedDirs[entry.Name()]; dropped {
				return fs.SkipDir
			}
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("locate %s under %s: %w", path, root, err)
		}
		name := filepath.ToSlash(rel)

		switch mode := entry.Type(); {
		case mode&fs.ModeSymlink != 0:
			// The target rather than the content: WalkDir does not follow
			// symlinks, and following one here would either escape the module or
			// digest the same bytes twice. Recording where it points still makes
			// a re-target visible.
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("read symlink %s: %w", name, err)
			}
			files = append(files, moduleFile{name: name, digest: "symlink:" + filepath.ToSlash(target)})
		case mode.IsRegular():
			if generatedState(entry.Name()) {
				return nil
			}
			fileSum, err := digestFile(path)
			if err != nil {
				return err
			}
			files = append(files, moduleFile{name: name, digest: fileSum})
		default:
			// A socket, fifo or device decides nothing about a Terraform result,
			// and reading one would block. Its presence is still recorded rather
			// than ignored outright.
			files = append(files, moduleFile{name: name, digest: "irregular:" + mode.String()})
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("read module directory %s: %w", dir, walkErr)
	}

	// WalkDir already visits lexically, but the order the digest depends on is
	// stated here rather than inherited: sort.Slice is a byte comparison, which
	// no locale collation on another runner can reorder.
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	return files, nil
}

// generatedState reports whether a file is Terraform state rather than module
// source.
//
// State is written by the very apply the fingerprint gates, so digesting it
// would move every stack's fingerprint on every deploy and re-apply everything
// forever — the §8 nondeterminism failure, self-inflicted. The default local
// backend puts terraform.tfstate directly in the root module, so this is not a
// hypothetical shape.
func generatedState(name string) bool {
	return name == ".terraform.tfstate.lock.info" ||
		strings.HasSuffix(name, ".tfstate") ||
		strings.HasSuffix(name, ".tfstate.backup")
}

// Combine folds a set of inputs, plus any extra scalar facts (the workspace
// name), into one fingerprint.
//
// The ordering is total, not merely by Name: names are a lossy projection of
// module keys (tf.ModuleName maps ".", "-", "/" and " " all to "_", so the keys
// "a-b" and "a.b" both become "a_b"), and sorting a set with equal keys leaves
// the order — and therefore the digest — unspecified. A digest that can flip
// between two runs over the same tree is exactly the failure this package
// promises not to have, so a collision is rejected outright rather than
// ordered around: the caller would also have silently dropped one of the two
// per-input output rows.
func Combine(inputs []Input, extras ...string) (string, error) {
	sorted := append([]Input(nil), inputs...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Name != sorted[j].Name {
			return sorted[i].Name < sorted[j].Name
		}
		return sorted[i].Identity < sorted[j].Identity
	})
	for i := 1; i < len(sorted); i++ {
		if sorted[i].Name == sorted[i-1].Name {
			return "", fmt.Errorf(
				"inputs %q and %q both reduce to the name %q; rename one of the module calls so the fingerprint is well defined",
				sorted[i-1].Identity, sorted[i].Identity, sorted[i].Name)
		}
	}

	sum := sha256.New()
	for _, extra := range extras {
		writeLine(sum, "extra")
		writeLine(sum, extra)
	}
	for _, in := range sorted {
		writeLine(sum, "input")
		writeLine(sum, in.Name)
		writeLine(sum, in.Identity)
		writeLine(sum, in.Digest)
	}
	return DigestPrefix + hexOf(sum.Sum(nil)), nil
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
