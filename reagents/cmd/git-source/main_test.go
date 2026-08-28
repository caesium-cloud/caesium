package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/reagents/internal/protocol"
)

// testRepo is a throwaway git repository on disk that the role clones over
// file://, which is what lets these tests run with no network at all.
type testRepo struct {
	t   *testing.T
	dir string
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	r := &testRepo{t: t, dir: t.TempDir()}
	r.git("init", "--quiet", "--initial-branch=main")
	r.write("stacks/network/main.tf", "resource \"null_resource\" \"n\" {}\n")
	r.write("stacks/app/main.tf", "resource \"null_resource\" \"a\" {}\n")
	r.write("modules/vpc/main.tf", "output \"vpc_id\" { value = \"vpc-1\" }\n")
	r.write("docs/notes.md", "not part of the pipeline\n")
	r.commit("initial")
	return r
}

func (r *testRepo) write(rel, contents string) {
	r.t.Helper()
	path := filepath.Join(r.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		r.t.Fatalf("write %s: %v", rel, err)
	}
}

func (r *testRepo) commit(msg string) string {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-m", msg)
	return strings.TrimSpace(r.git("rev-parse", "HEAD"))
}

func (r *testRepo) git(args ...string) string {
	r.t.Helper()
	full := append([]string{
		"-c", "user.name=Pack Test",
		"-c", "user.email=reagent@caesium.test",
		"-c", "commit.gpgsign=false",
		"-c", "safe.directory=" + r.dir,
	}, args...)
	cmd := exec.CommandContext(r.t.Context(), "git", full...)
	cmd.Dir = r.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func (r *testRepo) url() string { return "file://" + r.dir }

// runRole executes the role against cfg and returns the parsed output marker.
func runRole(t *testing.T, cfg config) (map[string]string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	e := protocol.New("git-source", &out, &errOut)
	err := materialize(context.Background(), cfg, e)
	if flushErr := e.Flush(); flushErr != nil {
		t.Fatalf("flush: %v", flushErr)
	}
	if err != nil {
		return nil, out.String(), err
	}
	line := strings.TrimRight(out.String(), "\n")
	payload, ok := strings.CutPrefix(line, protocol.OutputMarker+" ")
	if !ok {
		t.Fatalf("no output marker in %q", line)
	}
	var values map[string]string
	if jsonErr := json.Unmarshal([]byte(payload), &values); jsonErr != nil {
		t.Fatalf("unmarshal %q: %v", payload, jsonErr)
	}
	return values, out.String(), nil
}

func TestMaterializeEmitsCommitTreeDigestAndPath(t *testing.T) {
	repo := newTestRepo(t)
	want := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	dest := filepath.Join(t.TempDir(), "src")

	values, _, err := runRole(t, config{URL: repo.url(), Ref: "main", Dest: dest})
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if values["commit"] != want {
		t.Fatalf("commit = %q, want %q", values["commit"], want)
	}
	if !protocol.ValidDigest(values["treeDigest"]) {
		t.Fatalf("treeDigest = %q, want a sha256 digest", values["treeDigest"])
	}
	if values["path"] != dest {
		t.Fatalf("path = %q, want %q", values["path"], dest)
	}
	if _, statErr := os.Stat(filepath.Join(dest, "stacks", "network", "main.tf")); statErr != nil {
		t.Fatalf("tree was not staged: %v", statErr)
	}
}

func TestMaterializeAcceptsACommitSha(t *testing.T) {
	repo := newTestRepo(t)
	sha := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	dest := filepath.Join(t.TempDir(), "src")

	values, _, err := runRole(t, config{URL: repo.url(), Ref: sha, Dest: dest})
	if err != nil {
		t.Fatalf("materialize at sha: %v", err)
	}
	if values["commit"] != sha {
		t.Fatalf("commit = %q, want %q", values["commit"], sha)
	}
}

func TestTreeDigestIsStableAcrossClones(t *testing.T) {
	repo := newTestRepo(t)

	first, _, err := runRole(t, config{URL: repo.url(), Ref: "main", Dest: filepath.Join(t.TempDir(), "a")})
	if err != nil {
		t.Fatalf("first materialize: %v", err)
	}
	second, _, err := runRole(t, config{URL: repo.url(), Ref: "main", Dest: filepath.Join(t.TempDir(), "b")})
	if err != nil {
		t.Fatalf("second materialize: %v", err)
	}
	if first["treeDigest"] != second["treeDigest"] {
		t.Fatalf("treeDigest is not stable: %q vs %q", first["treeDigest"], second["treeDigest"])
	}
}

func TestTreeDigestCoversOnlyTheSparsePaths(t *testing.T) {
	repo := newTestRepo(t)
	sparse := []string{"stacks/**", "modules/**"}

	before, _, err := runRole(t, config{URL: repo.url(), Ref: "main", Sparse: sparse, Dest: filepath.Join(t.TempDir(), "a")})
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}

	// A change OUTSIDE the sparse paths moves the commit but must not move the
	// digest — that difference is the whole reason the role emits both.
	repo.write("docs/notes.md", "still not part of the pipeline\n")
	repo.commit("edit docs")

	afterDocs, _, err := runRole(t, config{URL: repo.url(), Ref: "main", Sparse: sparse, Dest: filepath.Join(t.TempDir(), "b")})
	if err != nil {
		t.Fatalf("materialize after doc edit: %v", err)
	}
	if afterDocs["commit"] == before["commit"] {
		t.Fatal("commit did not move after a new commit")
	}
	if afterDocs["treeDigest"] != before["treeDigest"] {
		t.Fatalf("treeDigest moved for a change outside the sparse paths: %q -> %q",
			before["treeDigest"], afterDocs["treeDigest"])
	}

	// A change INSIDE them must move it.
	repo.write("stacks/app/main.tf", "resource \"null_resource\" \"a\" { triggers = {} }\n")
	repo.commit("edit app")

	afterApp, _, err := runRole(t, config{URL: repo.url(), Ref: "main", Sparse: sparse, Dest: filepath.Join(t.TempDir(), "c")})
	if err != nil {
		t.Fatalf("materialize after stack edit: %v", err)
	}
	if afterApp["treeDigest"] == before["treeDigest"] {
		t.Fatal("treeDigest did not move for a change inside the sparse paths")
	}
}

func TestSparseCheckoutStagesOnlyTheRequestedPaths(t *testing.T) {
	repo := newTestRepo(t)
	dest := filepath.Join(t.TempDir(), "src")

	if _, _, err := runRole(t, config{URL: repo.url(), Ref: "main", Sparse: []string{"stacks/**"}, Dest: dest}); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "stacks", "app", "main.tf")); err != nil {
		t.Fatalf("sparse path was not staged: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "docs", "notes.md")); !os.IsNotExist(err) {
		t.Fatalf("excluded path was staged (err=%v)", err)
	}
}

func TestMaterializeFailsClosed(t *testing.T) {
	repo := newTestRepo(t)

	cases := map[string]config{
		"missing ref":       {URL: repo.url(), Ref: "no-such-ref", Dest: filepath.Join(t.TempDir(), "a")},
		"missing repo":      {URL: "file://" + filepath.Join(t.TempDir(), "absent"), Ref: "main", Dest: filepath.Join(t.TempDir(), "b")},
		"sparse matches -0": {URL: repo.url(), Ref: "main", Sparse: []string{"nothing/**"}, Dest: filepath.Join(t.TempDir(), "c")},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			_, stdout, err := runRole(t, cfg)
			if err == nil {
				t.Fatal("expected a failure")
			}
			if stdout != "" {
				t.Fatalf("failed run still wrote to stdout: %q", stdout)
			}
		})
	}
}

func TestMaterializeRefusesADirtyDestination(t *testing.T) {
	repo := newTestRepo(t)
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "leftover"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed dest: %v", err)
	}
	if _, _, err := runRole(t, config{URL: repo.url(), Ref: "main", Dest: dest}); err == nil {
		t.Fatal("expected a dirty destination to be refused")
	}
}

func TestLoadConfigDefaultsAndRequirements(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	if _, err := loadConfig(env(map[string]string{"GIT_REF": "main"})); err == nil {
		t.Fatal("missing GIT_URL accepted")
	}
	if _, err := loadConfig(env(map[string]string{"GIT_URL": "https://example.test/r.git"})); err == nil {
		t.Fatal("missing GIT_REF accepted")
	}
	if _, err := loadConfig(env(map[string]string{
		"GIT_URL": "https://example.test/r.git", "GIT_REF": "main", "DEST": "relative/path",
	})); err == nil {
		t.Fatal("relative DEST accepted")
	}

	cfg, err := loadConfig(env(map[string]string{
		"GIT_URL":    "https://example.test/r.git",
		"GIT_REF":    "main",
		"GIT_SPARSE": "  stacks/**   modules/**  ",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Dest != "/src" {
		t.Fatalf("DEST default = %q, want /src", cfg.Dest)
	}
	if strings.Join(cfg.Sparse, ",") != "stacks/**,modules/**" {
		t.Fatalf("GIT_SPARSE = %v", cfg.Sparse)
	}
}

func TestLoadConfigRejectsSparseNegation(t *testing.T) {
	_, err := loadConfig(func(k string) string {
		return map[string]string{
			"GIT_URL":    "https://example.test/r.git",
			"GIT_REF":    "main",
			"GIT_SPARSE": "stacks/** !stacks/legacy/**",
		}[k]
	})
	if err == nil || !strings.Contains(err.Error(), "negation") {
		t.Fatalf("negated sparse pattern = %v, want an explicit rejection", err)
	}
}

func TestGlobPathspec(t *testing.T) {
	if got := globPathspec("stacks/**"); got != ":(glob)stacks/**" {
		t.Fatalf("globPathspec = %q", got)
	}
	if got := globPathspec(":(exclude)docs/**"); got != ":(exclude)docs/**" {
		t.Fatalf("existing pathspec magic was rewritten: %q", got)
	}
}

func TestGitEnvPinsAnIdentitySoGitNeverResolvesTheHostname(t *testing.T) {
	env, cleanup, err := gitEnv(config{})
	defer cleanup()
	if err != nil {
		t.Fatalf("gitEnv: %v", err)
	}
	// Without these, fetch and checkout make git synthesize a reflog identity
	// by resolving the machine's own hostname — which blocks for the resolver
	// timeout, or fails, in a pod that has no DNS entry for itself.
	for _, want := range []string{
		"GIT_AUTHOR_NAME=", "GIT_AUTHOR_EMAIL=",
		"GIT_COMMITTER_NAME=", "GIT_COMMITTER_EMAIL=",
	} {
		found := false
		for _, kv := range env {
			if strings.HasPrefix(kv, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s is not pinned in the git environment: %v", want, env)
		}
	}
}

func TestSSHKeyLandsInAPrivateFileAndNeverInTheEnvironment(t *testing.T) {
	const secret = "-----BEGIN OPENSSH PRIVATE KEY-----\nnot-a-real-key\n-----END OPENSSH PRIVATE KEY-----"

	env, cleanup, err := gitEnv(config{SSHKey: secret, SSHKnownHosts: "example.test ssh-ed25519 AAAA"})
	defer cleanup()
	if err != nil {
		t.Fatalf("gitEnv: %v", err)
	}

	var sshCommand string
	for _, kv := range env {
		if strings.Contains(kv, secret) {
			t.Fatalf("the key leaked into the git environment: %q", kv)
		}
		if after, ok := strings.CutPrefix(kv, "GIT_SSH_COMMAND="); ok {
			sshCommand = after
		}
	}
	if sshCommand == "" {
		t.Fatal("GIT_SSH_COMMAND was not set")
	}
	if !strings.Contains(sshCommand, "StrictHostKeyChecking=yes") {
		t.Fatalf("known_hosts supplied but host-key checking is not strict: %q", sshCommand)
	}

	keyPath := ""
	fields := strings.Fields(sshCommand)
	for i, f := range fields {
		if f == "-i" && i+1 < len(fields) {
			keyPath = fields[i+1]
		}
	}
	if keyPath == "" {
		t.Fatalf("no identity file in %q", sshCommand)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file mode = %o, want 600", perm)
	}
	data, err := os.ReadFile(keyPath) //nolint:gosec // path produced by the code under test
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	if !strings.HasPrefix(string(data), secret) || !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("key file contents are wrong")
	}

	cleanup()
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("key file survived cleanup (err=%v)", err)
	}
}

func TestGitEnvNeverDisablesHostKeyChecking(t *testing.T) {
	env, cleanup, err := gitEnv(config{SSHKey: "key"})
	defer cleanup()
	if err != nil {
		t.Fatalf("gitEnv: %v", err)
	}
	for _, kv := range env {
		lowered := strings.ToLower(kv)
		for _, banned := range []string{"stricthostkeychecking=no", "stricthostkeychecking=accept-new", "userknownhostsfile=/dev/null"} {
			if strings.Contains(lowered, banned) {
				t.Fatalf("host-key checking was weakened: %q", kv)
			}
		}
	}
}

func TestLocalRepoPath(t *testing.T) {
	cases := map[string]struct {
		want string
		ok   bool
	}{
		"file:///srv/infra":            {"/srv/infra", true},
		"/srv/infra":                   {"/srv/infra", true},
		"https://example.test/r.git":   {"", false},
		"ssh://git@example.test/r.git": {"", false},
		"git@example.test:acme/r.git":  {"", false},
	}
	for url, want := range cases {
		got, ok := localRepoPath(url)
		if ok != want.ok || got != want.want {
			t.Fatalf("localRepoPath(%q) = (%q, %v), want (%q, %v)", url, got, ok, want.want, want.ok)
		}
	}
}

// TestLoadConfigRejectsOptionLikeValues guards the option-injection path. The
// reference manifest wires GIT_REF from a run parameter, so it is attacker-
// reachable in a way the manifest itself is not; `--upload-pack=<cmd>` against
// a local or file:// remote runs <cmd> in this container, beside the mounted
// deploy key.
func TestLoadConfigRejectsOptionLikeValues(t *testing.T) {
	cases := map[string]map[string]string{
		"option-like ref": {
			"GIT_URL": "https://example.test/r.git",
			"GIT_REF": "--upload-pack=sh -c 'id'",
		},
		"short option ref": {
			"GIT_URL": "https://example.test/r.git",
			"GIT_REF": "-c",
		},
		"option-like url": {
			"GIT_URL": "--upload-pack=sh -c 'id'",
			"GIT_REF": "main",
		},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadConfig(func(k string) string { return env[k] })
			if err == nil {
				t.Fatalf("%v was accepted; git would parse it as an option", env)
			}
			if !strings.Contains(err.Error(), "option") {
				t.Fatalf("error should explain why, got %v", err)
			}
		})
	}
}

// TestFetchPassesEndOfOptions is the second half of the option-injection
// guard: even a ref that reached the command some other way must be parsed as
// an operand, not a flag.
func TestFetchPassesEndOfOptions(t *testing.T) {
	args := fetchArgs("main")

	endIdx, refIdx := -1, -1
	for i, a := range args {
		switch a {
		case "--end-of-options":
			endIdx = i
		case "main":
			refIdx = i
		}
	}
	if endIdx < 0 {
		t.Fatalf("fetch is invoked without --end-of-options: %v", args)
	}
	if refIdx < 0 {
		t.Fatalf("the ref is missing from the fetch: %v", args)
	}
	// It has to come before the operands, or it protects nothing.
	if endIdx > refIdx {
		t.Fatalf("--end-of-options must precede the refspec: %v", args)
	}
	// A ref that looks like an option still lands after the separator.
	hostile := fetchArgs("--upload-pack=sh -c id")
	if hostile[len(hostile)-1] != "--upload-pack=sh -c id" {
		t.Fatalf("the ref is not the final operand: %v", hostile)
	}
}

// TestGitEnvRestrictsTransports closes the same class through the transport
// layer: `GIT_URL=ext::sh -c …` hands git an arbitrary command to run.
func TestGitEnvRestrictsTransports(t *testing.T) {
	env, cleanup, err := gitEnv(config{})
	defer cleanup()
	if err != nil {
		t.Fatalf("gitEnv: %v", err)
	}
	var allow string
	for _, kv := range env {
		if after, ok := strings.CutPrefix(kv, "GIT_ALLOW_PROTOCOL="); ok {
			allow = after
		}
	}
	if allow == "" {
		t.Fatalf("GIT_ALLOW_PROTOCOL is not set: %v", env)
	}
	for _, want := range []string{"file", "git", "http", "https", "ssh"} {
		if !slices.Contains(strings.Split(allow, ":"), want) {
			t.Fatalf("GIT_ALLOW_PROTOCOL %q does not permit %q", allow, want)
		}
	}
	if slices.Contains(strings.Split(allow, ":"), "ext") {
		t.Fatalf("GIT_ALLOW_PROTOCOL %q permits the ext transport, which runs an arbitrary command", allow)
	}
}

// TestErrorsNeverEchoURLCredentials is the credential-leak guard. The role
// takes care to keep GIT_SSH_KEY out of the logs; a token embedded in GIT_URL —
// the standard PAT form — must not reach the same place through an error
// string, because FailClosed writes those to the persisted task log.
func TestErrorsNeverEchoURLCredentials(t *testing.T) {
	const (
		user  = "x-access-token"
		token = "ghp_ThisIsTheSecretValue0000000000000000"
	)
	url := "https://" + user + ":" + token + "@127.0.0.1:1/acme/infra.git"

	_, stdout, err := runRole(t, config{URL: url, Ref: "main", Dest: filepath.Join(t.TempDir(), "src")})
	if err == nil {
		t.Fatal("the unreachable remote should have failed")
	}
	if stdout != "" {
		t.Fatalf("failed run wrote to stdout: %q", stdout)
	}

	// The error is what Emitter.FailClosed writes to the task log.
	var buf bytes.Buffer
	e := protocol.New("git-source", io.Discard, &buf)
	e.FailClosed(err)
	logged := buf.String()

	for _, secret := range []string{token, user + ":" + token} {
		if strings.Contains(logged, secret) {
			t.Fatalf("the credential reached the task log: %q", logged)
		}
	}
	if !strings.Contains(logged, "<redacted>") {
		t.Fatalf("the redaction marker is missing, so the URL may not have been scrubbed: %q", logged)
	}
	// The non-secret part must survive, or the message stops being useful.
	if !strings.Contains(logged, "127.0.0.1") {
		t.Fatalf("redaction removed the host too: %q", logged)
	}
}

func TestRedactURL(t *testing.T) {
	cases := map[string]string{
		"https://user:tok@example.test/r.git": "https://<redacted>@example.test/r.git",
		"https://tok@example.test/r.git":      "https://<redacted>@example.test/r.git",
		"https://example.test/r.git":          "https://example.test/r.git",
		"ssh://git@example.test/r.git":        "ssh://<redacted>@example.test/r.git",
		"file:///srv/infra":                   "file:///srv/infra",
		"/srv/infra":                          "/srv/infra",
	}
	for in, want := range cases {
		if got := redactURL(in); got != want {
			t.Fatalf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLoadConfigRejectsUnanchoredSparsePatterns keeps the checkout and the
// digest describing the same set of files. "*.tf" stages every .tf at any depth
// (gitignore semantics) but digests only the root-level ones (pathspec
// semantics anchored at the repo root) — a silent under-report.
func TestLoadConfigRejectsUnanchoredSparsePatterns(t *testing.T) {
	for _, pattern := range []string{"*.tf", "stacks", ":(glob)stacks/**", "/stacks/**", "!stacks/legacy/**"} {
		t.Run(pattern, func(t *testing.T) {
			_, err := loadConfig(func(k string) string {
				return map[string]string{
					"GIT_URL":    "https://example.test/r.git",
					"GIT_REF":    "main",
					"GIT_SPARSE": pattern,
				}[k]
			})
			if err == nil {
				t.Fatalf("GIT_SPARSE=%q was accepted", pattern)
			}
		})
	}

	// The canonical forms still work.
	for _, pattern := range []string{"stacks/**", "modules/**", "stacks/network/*.tf"} {
		if err := validSparsePattern(pattern); err != nil {
			t.Fatalf("validSparsePattern(%q) = %v, want accepted", pattern, err)
		}
	}
}
