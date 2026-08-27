// Package tf reads the parts of Terraform's on-disk state that the pack needs
// and that terraform-exec does not expose.
package tf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// DataDirName is Terraform's default working directory inside a root
	// module. TF_DATA_DIR relocates it, which is what lets discover run against
	// a read-only source mount — the design mounts src `readOnly: true` for
	// discover (§5.5), and `terraform get` has to write the manifest somewhere.
	DataDirName = ".terraform"

	// ManifestPath is where `terraform get` writes the module manifest,
	// relative to the data directory.
	ManifestPath = "modules/modules.json"
)

// VerifiedTerraformVersion records the Terraform release this parser was
// verified against. The manifest is an INTERNAL file with no documented
// compatibility promise — it is read anyway because it is the only surface that
// carries what a fingerprint needs (design §6.2): `terraform modules -json` is
// the supported introspection command, but its entries name a module by its
// local call name with no parent path and a source relative to a parent it
// never identifies, so an entry cannot be resolved to a directory and two
// different parents each declaring an `inner` are indistinguishable.
//
// The price of reading an unpromised file is paid by pinning the CLI version in
// the image and by ParseManifest rejecting any shape it does not recognise.
const VerifiedTerraformVersion = "1.15.9"

// Manifest is the parsed .terraform/modules/modules.json.
type Manifest struct {
	Modules []ModuleEntry `json:"Modules"`
}

// ModuleEntry is one installed module. Key is fully qualified ("tags.inner"),
// and Dir is already resolved relative to the root module directory — the two
// pieces `terraform modules -json` drops.
type ModuleEntry struct {
	Key     string `json:"Key"`
	Source  string `json:"Source"`
	Dir     string `json:"Dir"`
	Version string `json:"Version,omitempty"`
}

// ReadManifest loads and validates the manifest from a Terraform data
// directory: the default .terraform inside a root module, or wherever
// TF_DATA_DIR pointed.
func ReadManifest(dataDir string) (Manifest, error) {
	path := filepath.Join(dataDir, filepath.FromSlash(ManifestPath))
	data, err := os.ReadFile(path) //nolint:gosec // path derived from the caller's scan root.
	if err != nil {
		return Manifest{}, fmt.Errorf("read module manifest %s: %w", path, err)
	}
	m, err := ParseManifest(data)
	if err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// ParseManifest decodes the manifest strictly: an unknown field, a missing
// root entry, an entry with no Dir, or a duplicate Key is a hard failure.
//
// Strictness is the whole point. A silently mis-parsed manifest yields a
// fingerprint computed over the wrong set of directories, which is a stack that
// looks unchanged when it is not — the one failure this design must not have
// (§8). Because the image pins its Terraform version, a manifest shape that
// this parser does not recognise can only appear on a deliberate TF_VERSION
// bump, which is exactly when someone should be told.
func ParseManifest(data []byte) (Manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("unexpected module manifest shape (parser verified against Terraform %s): %w",
			VerifiedTerraformVersion, err)
	}
	if dec.More() {
		return Manifest{}, fmt.Errorf("unexpected trailing content in module manifest")
	}
	if len(m.Modules) == 0 {
		return Manifest{}, fmt.Errorf("module manifest lists no modules; even a module-free root module has one entry")
	}

	seen := make(map[string]struct{}, len(m.Modules))
	root := false
	for i, entry := range m.Modules {
		if strings.TrimSpace(entry.Dir) == "" {
			return Manifest{}, fmt.Errorf("module manifest entry %d (key %q) has no Dir", i, entry.Key)
		}
		if _, dup := seen[entry.Key]; dup {
			return Manifest{}, fmt.Errorf("module manifest lists key %q twice", entry.Key)
		}
		seen[entry.Key] = struct{}{}
		if entry.Key == "" {
			root = true
		}
	}
	if !root {
		return Manifest{}, fmt.Errorf("module manifest has no root entry (empty Key)")
	}
	return m, nil
}

// ModuleName is the stable label for one manifest entry: the root module is
// "root", and a nested key keeps its full path with the separators made safe
// for an output key (which becomes a CAESIUM_OUTPUT_* environment variable
// downstream).
func ModuleName(key string) string {
	if key == "" {
		return "root"
	}
	replacer := strings.NewReplacer(".", "_", "-", "_", "/", "_", " ", "_")
	return replacer.Replace(key)
}

// ResolvedModule is one manifest entry resolved into the two things the
// fingerprint needs: where to read the module from, and what to call it.
type ResolvedModule struct {
	// Key is the manifest's fully-qualified module key ("" for the root).
	Key string
	// Dir is the filesystem path to read. It is derived from the manifest and
	// never enters a digest.
	Dir string
	// Identity is the path-independent label that DOES enter the digest. For a
	// module living in the source tree it is the relative directory; for one
	// Terraform installed into the data directory it is the declared source
	// (plus version), because the install path is a per-run scratch location.
	Identity string
	// Installed reports whether Terraform fetched this module into the data
	// directory (a registry, git or http source) rather than reading it from
	// the source tree.
	Installed bool
}

// Resolve turns every manifest entry into a ResolvedModule.
//
// Terraform records a local module's Dir relative to the root module, but an
// installed module's Dir points into TF_DATA_DIR — and when TF_DATA_DIR is an
// absolute path (which discover sets so it can run against a read-only source
// mount) that Dir is absolute too. Verified against Terraform 1.15.9:
//
//	default data dir:         {"Key":"remote","Source":"git::file:///tmp/sub","Dir":".terraform/modules/remote"}
//	TF_DATA_DIR=/tmp/absdata: {"Key":"remote","Source":"git::file:///tmp/sub","Dir":"/tmp/absdata/modules/remote"}
//
// So joining Dir onto the root module blindly yields a path that does not exist
// — which would make discover exit 1 on the most common Terraform repo shape
// there is — and folding Dir into the digest would put a per-run scratch path
// into the fingerprint, the §8 nondeterminism failure. An installed module's
// stable identity is its Source and Version; only its *content* digest varies.
func (m Manifest) Resolve(rootDir, dataDir string) ([]ResolvedModule, error) {
	absData, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve data directory %s: %w", dataDir, err)
	}

	resolved := make([]ResolvedModule, 0, len(m.Modules))
	for _, entry := range m.Modules {
		dir := entry.Dir
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(rootDir, filepath.FromSlash(entry.Dir))
		}
		absDir, err := filepath.Abs(dir)
		if err != nil {
			return nil, fmt.Errorf("module %q: resolve %s: %w", entry.Key, entry.Dir, err)
		}

		installed := withinDir(absData, absDir)
		identity := filepath.ToSlash(filepath.Clean(entry.Dir))
		switch {
		case installed:
			// The install path is scratch. Source is what the configuration
			// actually declared, and it is what a reader would recognise.
			source := strings.TrimSpace(entry.Source)
			if source == "" {
				return nil, fmt.Errorf(
					"module %q was installed into the Terraform data directory but declares no source; "+
						"its identity cannot be made independent of the install path", entry.Key)
			}
			identity = source
			if v := strings.TrimSpace(entry.Version); v != "" {
				identity += "@" + v
			}
		case filepath.IsAbs(entry.Dir):
			// A local module addressed absolutely would put a machine-specific
			// path into the fingerprint, so two workers with different checkout
			// roots would disagree. Fail closed rather than digest it.
			return nil, fmt.Errorf(
				"module %q has the absolute directory %s outside the Terraform data directory; "+
					"the fingerprint requires relative paths", entry.Key, entry.Dir)
		}

		resolved = append(resolved, ResolvedModule{
			Key:       entry.Key,
			Dir:       absDir,
			Identity:  identity,
			Installed: installed,
		})
	}
	return resolved, nil
}

// withinDir reports whether path is base or lives underneath it. Both arguments
// must already be absolute.
func withinDir(base, path string) bool {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
