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
