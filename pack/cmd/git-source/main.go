// Command git-source is the materialize role of the unit-pipeline pattern
// (design §5.1, §6.1): it pins a source tree at a ref, stages it on a shared
// volume, and emits that tree's identity.
//
// It is deliberately not Terraform-aware. Every binding of the pattern — dbt,
// a monorepo, database migrations — materializes its source the same way, so
// this image is the one role all of them share.
//
// Environment:
//
//	GIT_URL             repository to clone (https://, ssh://, git@host:path, file://)
//	GIT_REF             the ref to pin: a branch, tag or full commit sha (required)
//	GIT_SPARSE          space-separated sparse-checkout patterns; empty = whole tree
//	GIT_SSH_KEY         private key, already resolved from a secret:// URI by Caesium
//	GIT_SSH_KNOWN_HOSTS known_hosts content for the forge, enabling strict host-key checking
//	DEST                where to stage the tree (default /src)
//
// Emits ##caesium::output {commit, treeDigest, path}.
//
// treeDigest is a sha256 over `git ls-tree -r <ref>` restricted to the sparse
// paths, sorted. Using git's own object store means an exact content digest of
// exactly the paths the pipeline consumes, without reading a single file byte —
// and, unlike the commit sha, it is identical for two commits that did not
// touch those paths.
//
// Every failure is fatal and emits nothing: a downstream step must never see a
// partial identity for a tree that was not fully staged.
package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/caesium-cloud/caesium/pack/internal/protocol"
)

const roleName = "git-source"

func main() {
	protocol.Run(roleName, func(e *protocol.Emitter) error {
		cfg, err := loadConfig(os.Getenv)
		if err != nil {
			return err
		}
		return materialize(context.Background(), cfg, e)
	})
}

// config is the resolved environment. SSHKey and SSHKnownHosts are secret
// material: they are written to files and never rendered into an error, a log
// line or a command argument.
type config struct {
	URL           string
	Ref           string
	Sparse        []string
	SSHKey        string
	SSHKnownHosts string
	Dest          string
}

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		URL:           strings.TrimSpace(getenv("GIT_URL")),
		Ref:           strings.TrimSpace(getenv("GIT_REF")),
		Sparse:        strings.Fields(getenv("GIT_SPARSE")),
		SSHKey:        getenv("GIT_SSH_KEY"),
		SSHKnownHosts: getenv("GIT_SSH_KNOWN_HOSTS"),
		Dest:          strings.TrimSpace(getenv("DEST")),
	}
	if cfg.URL == "" {
		return config{}, fmt.Errorf("GIT_URL is required")
	}
	// A ref is required rather than defaulted to the remote's HEAD: "whatever
	// the default branch points at right now" is precisely the unpinned input
	// this role exists to remove.
	if cfg.Ref == "" {
		return config{}, fmt.Errorf("GIT_REF is required (a branch, tag or commit sha)")
	}
	if cfg.Dest == "" {
		cfg.Dest = "/src"
	}
	if !filepath.IsAbs(cfg.Dest) {
		return config{}, fmt.Errorf("DEST %q must be an absolute path", cfg.Dest)
	}
	for _, pattern := range cfg.Sparse {
		// A negation is a sparse-checkout concept with no pathspec equivalent,
		// so honouring it in the checkout while ignoring it in the digest would
		// silently describe a different tree than the one that was staged.
		if strings.HasPrefix(pattern, "!") {
			return config{}, fmt.Errorf("GIT_SPARSE pattern %q: negation is not supported; list the paths to include", pattern)
		}
	}
	return cfg, nil
}

func materialize(ctx context.Context, cfg config, e *protocol.Emitter) error {
	env, cleanup, err := gitEnv(cfg)
	defer cleanup()
	if err != nil {
		return err
	}

	git := &gitRunner{dir: cfg.Dest, env: env, safeDirs: localSafeDirs(cfg.URL, cfg.Dest)}

	if err := os.MkdirAll(cfg.Dest, 0o755); err != nil {
		return fmt.Errorf("create DEST %s: %w", cfg.Dest, err)
	}
	if entries, err := os.ReadDir(cfg.Dest); err != nil {
		return fmt.Errorf("read DEST %s: %w", cfg.Dest, err)
	} else if len(entries) > 0 {
		// A dirty destination means a previous run's tree is still there, so
		// the digest would describe something other than what was fetched.
		return fmt.Errorf("DEST %s is not empty; the materialize role requires a clean destination", cfg.Dest)
	}

	if _, err := git.run(ctx, "init", "--quiet", "--initial-branch=caesium-source"); err != nil {
		return err
	}
	if _, err := git.run(ctx, "remote", "add", "origin", cfg.URL); err != nil {
		return err
	}
	if len(cfg.Sparse) > 0 {
		// --no-cone: the patterns are gitignore-style globs ("stacks/**"), not
		// the directory prefixes cone mode accepts.
		args := append([]string{"sparse-checkout", "set", "--no-cone", "--"}, cfg.Sparse...)
		if _, err := git.run(ctx, args...); err != nil {
			return err
		}
	}
	if _, err := git.run(ctx, "fetch", "--depth=1", "--no-tags", "origin", cfg.Ref); err != nil {
		return fmt.Errorf("fetch %s at %s: %w", cfg.URL, cfg.Ref, err)
	}
	if _, err := git.run(ctx, "checkout", "--detach", "FETCH_HEAD"); err != nil {
		return err
	}

	commit, err := git.run(ctx, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	commit = strings.TrimSpace(commit)
	if len(commit) != 40 && len(commit) != 64 {
		return fmt.Errorf("resolved commit %q is not a git object id", commit)
	}

	digest, err := treeDigest(ctx, git, cfg.Sparse)
	if err != nil {
		return err
	}

	return e.Output(map[string]string{
		"commit":     commit,
		"treeDigest": digest,
		"path":       cfg.Dest,
	})
}

// treeDigest hashes the staged tree by way of git's own object store: every
// index entry (mode, blob sha, path) for the sparse paths, sorted, joined with
// newlines. The clone is fresh into an empty destination, so the index is
// exactly the tree at the pinned commit. Each entry already carries the blob's
// sha, so the digest is exact content addressing without opening a file — and
// it is stable across clones, checkout order, and filesystem timestamps.
//
// `git ls-files`, not the `ls-tree` the design sketches: ls-tree does not
// support pathspec magic and does not glob, so `stacks/**` silently matches
// nothing there. ls-files takes `:(glob)`, whose `**` is the same wildcard the
// sparse-checkout patterns use, so the digest covers exactly the paths that
// were staged.
func treeDigest(ctx context.Context, git *gitRunner, sparse []string) (string, error) {
	args := []string{"ls-files", "-s", "-z"}
	if len(sparse) > 0 {
		args = append(args, "--")
		for _, pattern := range sparse {
			args = append(args, globPathspec(pattern))
		}
	}
	out, err := git.run(ctx, args...)
	if err != nil {
		return "", err
	}

	entries := make([]string, 0, 64)
	for _, entry := range strings.Split(out, "\x00") {
		if entry != "" {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		// An empty result means the sparse patterns matched nothing. Emitting a
		// digest of "" would make every such misconfiguration look like one
		// stable, unchanging tree.
		return "", fmt.Errorf("no tree entries matched %v; nothing was staged", sparse)
	}
	// git already emits sorted output, but sorting here makes the digest
	// independent of that promise, and sort.Strings is a byte comparison — no
	// locale collation can reorder it on a different runner.
	sort.Strings(entries)

	sum := sha256.New()
	for _, entry := range entries {
		sum.Write([]byte(entry))
		sum.Write([]byte("\n"))
	}
	return protocol.Digest(sum.Sum(nil)), nil
}

// globPathspec turns one GIT_SPARSE pattern into the pathspec that selects the
// same paths. A pattern that already carries pathspec magic (":(exclude)…") is
// passed through untouched.
func globPathspec(pattern string) string {
	if strings.HasPrefix(pattern, ":") {
		return pattern
	}
	return ":(glob)" + pattern
}

// gitRunner invokes git with a fixed working directory and a scrubbed
// environment.
type gitRunner struct {
	dir      string
	env      []string
	safeDirs []string
}

func (g *gitRunner) run(ctx context.Context, args ...string) (string, error) {
	full := make([]string, 0, len(args)+2*len(g.safeDirs)+2)
	for _, dir := range g.safeDirs {
		// Scoped to the paths this invocation actually touches — never "*".
		// A local clone source is typically owned by a different uid than the
		// unprivileged user this image runs as, which git refuses to read
		// without being told the path is intended.
		full = append(full, "-c", "safe.directory="+dir)
	}
	full = append(full, args...)

	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = g.dir
	cmd.Env = g.env

	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// localSafeDirs returns the filesystem paths git must be told are intentional.
func localSafeDirs(url, dest string) []string {
	dirs := []string{dest}
	if path, ok := localRepoPath(url); ok {
		dirs = append(dirs, path)
	}
	return dirs
}

// localRepoPath resolves a file:// URL or a bare absolute path to a local
// directory. Anything else (https, ssh, git@host:path) is remote.
func localRepoPath(url string) (string, bool) {
	switch {
	case strings.HasPrefix(url, "file://"):
		return filepath.Clean(strings.TrimPrefix(url, "file://")), true
	case filepath.IsAbs(url) && !strings.Contains(url, "://"):
		return filepath.Clean(url), true
	default:
		return "", false
	}
}

// gitEnv builds the environment git runs with, materializing GIT_SSH_KEY (and,
// when supplied, GIT_SSH_KNOWN_HOSTS) into 0600 files.
//
// Two properties matter. The key never appears in a command line, an error, or
// the inherited environment of the git subprocess — only in a file the process
// deletes on the way out. And host-key checking is never disabled: with a
// known_hosts supplied, checking is strict against it; without one, ssh's own
// default applies and an unknown host fails rather than being trusted.
func gitEnv(cfg config) ([]string, func(), error) {
	// Start from a scrubbed environment: git inherits only what it needs, so a
	// resolved secret in the role's own environment cannot leak into a
	// subprocess (or into `git config --show-origin` style diagnostics).
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_ADVICE=0",
		// A fixed identity, because fetch and checkout write reflog entries and
		// git otherwise synthesizes an address by resolving the machine's own
		// hostname. In a pod with no DNS for its name that lookup blocks until
		// the resolver gives up — measured at 5 s per fetch on a
		// network-isolated container — or fails outright. This role never
		// creates a commit, so the value itself is inert.
		"GIT_AUTHOR_NAME=caesium git-source",
		"GIT_AUTHOR_EMAIL=git-source@caesium.invalid",
		"GIT_COMMITTER_NAME=caesium git-source",
		"GIT_COMMITTER_EMAIL=git-source@caesium.invalid",
	}
	if cfg.SSHKey == "" {
		return env, func() {}, nil
	}

	dir, err := os.MkdirTemp("", "git-source-ssh-")
	if err != nil {
		return env, func() {}, fmt.Errorf("create ssh key directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := os.Chmod(dir, 0o700); err != nil {
		return env, cleanup, fmt.Errorf("secure ssh key directory: %w", err)
	}

	keyPath := filepath.Join(dir, "id")
	key := cfg.SSHKey
	if !strings.HasSuffix(key, "\n") {
		// OpenSSH rejects a key file without a trailing newline.
		key += "\n"
	}
	if err := os.WriteFile(keyPath, []byte(key), 0o600); err != nil {
		return env, cleanup, fmt.Errorf("write ssh key: %w", err)
	}

	ssh := []string{"ssh", "-i", keyPath, "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes"}
	if strings.TrimSpace(cfg.SSHKnownHosts) != "" {
		hostsPath := filepath.Join(dir, "known_hosts")
		hosts := cfg.SSHKnownHosts
		if !strings.HasSuffix(hosts, "\n") {
			hosts += "\n"
		}
		if err := os.WriteFile(hostsPath, []byte(hosts), 0o600); err != nil {
			return env, cleanup, fmt.Errorf("write known_hosts: %w", err)
		}
		ssh = append(ssh,
			"-o", "UserKnownHostsFile="+hostsPath,
			"-o", "StrictHostKeyChecking=yes",
		)
	}
	env = append(env, "GIT_SSH_COMMAND="+strings.Join(ssh, " "))
	return env, cleanup, nil
}
