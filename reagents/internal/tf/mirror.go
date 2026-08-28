package tf

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// LockFileName is the dependency lock file Terraform writes next to a root
// module. It is the only file that records the exact provider versions and
// package checksums a stack resolved to, which is what makes the warm step's
// mirror key content-addressed rather than a guess (design §6.3, §6.5).
const LockFileName = ".terraform.lock.hcl"

// LockedProvider is one `provider` block of a .terraform.lock.hcl.
type LockedProvider struct {
	// Source is the fully-qualified provider address as the lock file spells
	// it, e.g. "registry.terraform.io/hashicorp/null".
	Source string
	// Version is the exact resolved version, e.g. "3.3.1".
	Version string
	// Hashes are the recorded package checksums ("h1:…", "zh:…"), sorted.
	Hashes []string
}

// sourceAddressPattern is the shape a provider source address may take:
// <hostname>/<namespace>/<type> or <namespace>/<type>, each segment limited to
// the characters Terraform's own address parser accepts.
//
// The address is interpolated into generated HCL (see RequiredProvidersHCL), so
// validating it here is what keeps a hostile lock file from injecting
// configuration into the synthetic root module the mirror runs against.
var sourceAddressPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?(/[a-z0-9]([a-z0-9._-]*[a-z0-9])?){1,2}$`)

// versionPattern is a Terraform provider version. Same reasoning as
// sourceAddressPattern: it reaches generated HCL.
var versionPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*([.+-][0-9A-Za-z.+-]+)?$`)

// hashPattern is one recorded checksum. Hashes never reach generated HCL, but a
// malformed one would silently weaken the mirror key, so the shape is enforced.
var hashPattern = regexp.MustCompile(`^[a-z0-9]+:[A-Za-z0-9+/=_-]+$`)

// Namespace returns the namespace segment of the provider's source address
// ("hashicorp" for "registry.terraform.io/hashicorp/null").
func (p LockedProvider) Namespace() string {
	parts := strings.Split(p.Source, "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-2]
}

// Type returns the type segment of the provider's source address ("null" for
// "registry.terraform.io/hashicorp/null"). It is also the conventional local
// name a configuration refers to the provider by.
func (p LockedProvider) Type() string {
	parts := strings.Split(p.Source, "/")
	return parts[len(parts)-1]
}

// FindLockFiles walks root and returns every .terraform.lock.hcl beneath it, in
// sorted order. Version-control and Terraform working directories are skipped:
// a stale lock file inside a .terraform/ directory belongs to a previous run,
// not to the configuration.
func FindLockFiles(root string) ([]string, error) {
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", DataDirName:
				if path == root {
					return nil
				}
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == LockFileName {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan %s for %s: %w", root, LockFileName, err)
	}
	sort.Strings(found)
	return found, nil
}

// ReadLockFile parses one lock file from disk.
func ReadLockFile(path string) ([]LockedProvider, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path comes from a walk of the caller's source root.
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	providers, err := ParseLockFile(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return providers, nil
}

// ParseLockFile decodes a .terraform.lock.hcl strictly.
//
// The lock file is HCL, but the reagents deliberately do not depend on an HCL
// parser: the file is machine-generated with a fixed shape, and the alternative
// — pulling hashicorp/hcl into the reagents — buys nothing the strictness below
// does not. Strictness is the point. A silently mis-parsed lock file yields a
// mirror key that does not track the provider set, so a changed provider
// version would reuse a mirror that does not contain it and every `init` would
// fail offline — or, worse, a stale marker would make warm exit fast against a
// mirror missing the provider. Anything this parser does not recognise is an
// error naming the line.
func ParseLockFile(data []byte) ([]LockedProvider, error) {
	var (
		providers []LockedProvider
		current   *LockedProvider
		inHashes  bool
		seen      = map[string]struct{}{}
	)

	for i, raw := range strings.Split(string(data), "\n") {
		lineNo := i + 1
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if inHashes {
			if line == "]" {
				inHashes = false
				continue
			}
			hash, err := parseQuotedListEntry(line)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNo, err)
			}
			if !hashPattern.MatchString(hash) {
				return nil, fmt.Errorf("line %d: %q is not a <scheme>:<value> provider hash", lineNo, hash)
			}
			current.Hashes = append(current.Hashes, hash)
			continue
		}

		if current == nil {
			source, err := parseProviderBlockHeader(line)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNo, err)
			}
			if _, dup := seen[source]; dup {
				return nil, fmt.Errorf("line %d: provider %q is declared twice", lineNo, source)
			}
			seen[source] = struct{}{}
			current = &LockedProvider{Source: source}
			continue
		}

		if line == "}" {
			if current.Version == "" {
				return nil, fmt.Errorf("line %d: provider %q has no version", lineNo, current.Source)
			}
			sort.Strings(current.Hashes)
			providers = append(providers, *current)
			current = nil
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: unexpected content %q inside provider %q", lineNo, line, current.Source)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "version":
			v, err := unquote(value)
			if err != nil {
				return nil, fmt.Errorf("line %d: version: %w", lineNo, err)
			}
			if !versionPattern.MatchString(v) {
				return nil, fmt.Errorf("line %d: %q is not a provider version", lineNo, v)
			}
			current.Version = v
		case "constraints":
			// Recorded for humans; it does not enter the mirror key, because
			// two lock files that resolved the same version from different
			// constraints need the same mirror.
			if _, err := unquote(value); err != nil {
				return nil, fmt.Errorf("line %d: constraints: %w", lineNo, err)
			}
		case "hashes":
			if value == "[" {
				inHashes = true
				continue
			}
			hashes, err := parseInlineHashList(value)
			if err != nil {
				return nil, fmt.Errorf("line %d: hashes: %w", lineNo, err)
			}
			for _, h := range hashes {
				if !hashPattern.MatchString(h) {
					return nil, fmt.Errorf("line %d: %q is not a <scheme>:<value> provider hash", lineNo, h)
				}
			}
			current.Hashes = append(current.Hashes, hashes...)
		default:
			return nil, fmt.Errorf("line %d: unknown attribute %q in provider %q "+
				"(lock-file parser verified against Terraform %s)", lineNo, key, current.Source, VerifiedTerraformVersion)
		}
	}

	if current != nil || inHashes {
		return nil, fmt.Errorf("unterminated provider block")
	}
	if len(providers) == 0 {
		return nil, fmt.Errorf("lock file declares no providers")
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].Source < providers[j].Source })
	return providers, nil
}

func parseProviderBlockHeader(line string) (string, error) {
	rest, ok := strings.CutPrefix(line, "provider ")
	if !ok {
		return "", fmt.Errorf("expected a `provider \"…\" {` block, got %q", line)
	}
	rest, ok = strings.CutSuffix(strings.TrimSpace(rest), "{")
	if !ok {
		return "", fmt.Errorf("expected a `provider \"…\" {` block, got %q", line)
	}
	source, err := unquote(strings.TrimSpace(rest))
	if err != nil {
		return "", err
	}
	if !sourceAddressPattern.MatchString(source) {
		return "", fmt.Errorf("%q is not a provider source address", source)
	}
	return source, nil
}

// parseQuotedListEntry reads one `"value",` element of a multi-line HCL list.
func parseQuotedListEntry(line string) (string, error) {
	return unquote(strings.TrimSuffix(line, ","))
}

// parseInlineHashList reads the single-line `["a", "b"]` list form.
func parseInlineHashList(value string) ([]string, error) {
	inner, ok := strings.CutPrefix(value, "[")
	if !ok {
		return nil, fmt.Errorf("expected a list, got %q", value)
	}
	inner, ok = strings.CutSuffix(strings.TrimSpace(inner), "]")
	if !ok {
		return nil, fmt.Errorf("expected a list, got %q", value)
	}
	inner = strings.TrimSpace(inner)
	if inner == "" {
		return nil, nil
	}
	var out []string
	for _, part := range strings.Split(inner, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		h, err := unquote(part)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, nil
}

// unquote reads an HCL string literal. Escapes are rejected rather than
// decoded: no field this parser reads is ever generated with one, so an escape
// means the file is not the shape this parser was verified against.
func unquote(s string) (string, error) {
	if len(s) < 2 || !strings.HasPrefix(s, `"`) || !strings.HasSuffix(s, `"`) {
		return "", fmt.Errorf("expected a quoted string, got %q", s)
	}
	inner := s[1 : len(s)-1]
	if strings.ContainsAny(inner, "\"\\") {
		return "", fmt.Errorf("quoted string %q contains an escape this parser does not decode", s)
	}
	return inner, nil
}

// MergeLocked folds several lock files into the union the mirror must contain.
//
// A provider pinned to two different versions by two stacks yields two entries:
// the mirror has to hold both, or the stack on the older pin cannot init
// offline. Two entries for the same source AND version are merged, and their
// hash sets unioned — the same package recorded from two platforms is one
// package.
func MergeLocked(sets ...[]LockedProvider) []LockedProvider {
	type key struct{ source, version string }
	merged := map[key]map[string]struct{}{}
	for _, set := range sets {
		for _, p := range set {
			k := key{p.Source, p.Version}
			if merged[k] == nil {
				merged[k] = map[string]struct{}{}
			}
			for _, h := range p.Hashes {
				merged[k][h] = struct{}{}
			}
		}
	}

	out := make([]LockedProvider, 0, len(merged))
	for k, hashes := range merged {
		p := LockedProvider{Source: k.source, Version: k.version, Hashes: make([]string, 0, len(hashes))}
		for h := range hashes {
			p.Hashes = append(p.Hashes, h)
		}
		sort.Strings(p.Hashes)
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].Version < out[j].Version
	})
	return out
}

// MirrorKey digests the sorted provider/version/hash union plus the target
// platforms into the cache key the warm step is addressed by (design §6.3).
//
// Two properties matter. It must move when the provider set moves, or a warm
// step would exit fast on a marker for a mirror that does not contain what the
// new lock file needs. And it must NOT move for the same set discovered in a
// different traversal order, or every run would re-mirror. Both are why the
// input is a sorted union rather than a digest of the files themselves — two
// stacks that pin the same provider produce one mirror.
//
// The platforms are part of the key because a mirror populated for linux_amd64
// is useless to an arm64 runner, and the marker must not claim otherwise.
func MirrorKey(providers []LockedProvider, platforms []string) string {
	sum := sha256.New()
	writeKeyLine(sum, "mirror-key-v1")
	for _, platform := range sortedCopy(platforms) {
		writeKeyLine(sum, "platform")
		writeKeyLine(sum, platform)
	}
	for _, p := range MergeLocked(providers) {
		writeKeyLine(sum, "provider")
		writeKeyLine(sum, p.Source)
		writeKeyLine(sum, p.Version)
		for _, h := range p.Hashes {
			writeKeyLine(sum, "hash")
			writeKeyLine(sum, h)
		}
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// RequiredProvidersHCL renders a synthetic root module that requires exactly
// the given providers at exactly their locked versions.
//
// `terraform providers mirror` refuses to run against a configuration whose
// modules are not installed, and installing them would mean running
// `terraform get` over every stack — which needs the module sources to resolve
// and writes into a data directory per stack. The lock file already names the
// complete resolved provider set, so a synthetic root module carrying only
// `required_providers` mirrors precisely the same packages without touching the
// real configuration at all. That is also what keeps the warm role's `src`
// mount read-only (design §5.5): nothing is ever written into the source tree.
func RequiredProvidersHCL(providers []LockedProvider) (string, error) {
	if len(providers) == 0 {
		return "", fmt.Errorf("no providers to mirror")
	}

	names, err := localNames(providers)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("# Generated by caesium tf-warm. Not a real configuration: it exists only so\n")
	b.WriteString("# `terraform providers mirror` has a required_providers set to mirror.\n")
	b.WriteString("terraform {\n  required_providers {\n")
	for i, p := range providers {
		if !sourceAddressPattern.MatchString(p.Source) {
			return "", fmt.Errorf("provider source %q is not a provider source address", p.Source)
		}
		if !versionPattern.MatchString(p.Version) {
			return "", fmt.Errorf("provider %q version %q is not a version", p.Source, p.Version)
		}
		fmt.Fprintf(&b, "    %s = {\n      source  = %q\n      version = \"= %s\"\n    }\n", names[i], p.Source, p.Version)
	}
	b.WriteString("  }\n}\n")
	return b.String(), nil
}

// localNames assigns each provider a unique HCL local name. The conventional
// name is the type segment; when two namespaces offer the same type they are
// disambiguated by namespace, deterministically, so the generated file is
// byte-stable for a given provider set.
func localNames(providers []LockedProvider) ([]string, error) {
	counts := map[string]int{}
	for _, p := range providers {
		counts[p.Type()]++
	}
	names := make([]string, len(providers))
	used := map[string]struct{}{}
	for i, p := range providers {
		name := p.Type()
		if counts[name] > 1 {
			name = p.Namespace() + "_" + p.Type()
		}
		name = strings.ReplaceAll(name, ".", "_")
		name = strings.ReplaceAll(name, "-", "_")
		if !localNamePattern.MatchString(name) {
			return nil, fmt.Errorf("provider %q has no usable local name (derived %q)", p.Source, name)
		}
		if _, dup := used[name]; dup {
			return nil, fmt.Errorf("providers %q and an earlier entry both reduce to the local name %q", p.Source, name)
		}
		used[name] = struct{}{}
		names[i] = name
	}
	return names, nil
}

var localNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// TerraformRC renders the CLI configuration every consuming step points
// TF_CLI_CONFIG_FILE at.
//
// `direct` is excluded rather than omitted. Omitting it leaves Terraform's
// default installation methods in place, so an `init` whose provider is missing
// from the mirror would quietly reach the public registry instead of failing —
// which is the difference between a hermetic run and one that only looks
// hermetic until the network is gone (design §3.4).
func TerraformRC(mirrorPath string) string {
	return fmt.Sprintf(`# Generated by caesium tf-warm (design §3.4, §6.3).
#
# Consuming steps mount the cache volume read-only and install providers from
# this filesystem mirror alone. `+"`direct`"+` is excluded so a provider missing from
# the mirror fails the init instead of silently reaching the public registry.
provider_installation {
  filesystem_mirror {
    path    = %q
    include = ["*/*/*"]
  }
  direct {
    exclude = ["*/*/*"]
  }
}
`, mirrorPath)
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func writeKeyLine(w interface{ Write([]byte) (int, error) }, s string) {
	_, _ = w.Write([]byte(s))
	_, _ = w.Write([]byte("\n"))
}
