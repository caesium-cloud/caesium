//go:build integration

// git-source is an isolated qualification fixture. Git itself supplies every
// advertised ref and pack byte; this wrapper only fences the daemon request.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const manifestPath = "jobs/imported.job.yaml"

var identifier = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,99}$`)
var commitID = regexp.MustCompile(`^[0-9a-f]{40}$`)

type fixtureState struct {
	SchemaVersion int    `json:"schema_version"`
	Alias         string `json:"alias"`
	SourceID      string `json:"source_id"`
	URL           string `json:"url"`
	Ref           string `json:"ref"`
	Path          string `json:"path"`
	Image         string `json:"image"`
	InitialCommit string `json:"initial_commit"`
	GitVersion    string `json:"git_version"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		// Never echo Git's diagnostics, repository bytes or request payloads.
		fmt.Fprintln(os.Stderr, "Git fixture refused or failed:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("expected init or serve")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	repo := fs.String("repo", "", "fresh owned repository path")
	alias := fs.String("alias", "", "owned job alias (init)")
	sourceID := fs.String("source-id", "", "owned source identity (init)")
	image := fs.String("image", "", "already-loaded task image (init)")
	gitURL := fs.String("url", "", "private git:// alias:9418/coverage.git")
	listen := fs.String("listen", "0.0.0.0:9418", "private container TCP bind (serve)")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return errors.New("invalid fixture arguments")
	}
	if err := validateRepoPath(*repo); err != nil {
		return err
	}
	switch args[0] {
	case "init":
		state := fixtureState{SchemaVersion: 1, Alias: *alias, SourceID: *sourceID,
			URL: *gitURL, Ref: "main", Path: manifestPath, Image: *image}
		return initialize(ctx, *repo, state)
	case "serve":
		state, err := readState(filepath.Join(filepath.Dir(*repo), "state.json"))
		if err != nil || state.URL != *gitURL {
			return errors.New("missing or mismatched fixture state")
		}
		if err := validateRepository(ctx, *repo, state); err != nil {
			return err
		}
		listenConfig := net.ListenConfig{}
		listener, err := listenConfig.Listen(ctx, "tcp", *listen)
		if err != nil {
			return errors.New("cannot bind private fixture listener")
		}
		defer listener.Close()
		fmt.Println(`{"phase":"git-fixture-ready"}`)
		return serve(ctx, listener, *repo, state.URL)
	default:
		return errors.New("unknown fixture command")
	}
}

func validateRepoPath(repo string) error {
	if !filepath.IsAbs(repo) || filepath.Clean(repo) != repo || filepath.Base(repo) != "coverage.git" {
		return errors.New("repository must be an absolute owned coverage.git path")
	}
	parent := filepath.Dir(repo)
	real, err := filepath.EvalSymlinks(parent)
	if err != nil || real != parent || parent == string(filepath.Separator) {
		return errors.New("repository parent must be an existing private real directory")
	}
	if info, err := os.Lstat(repo); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return errors.New("repository must not be a symlink or non-directory")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot inspect repository")
	}
	return nil
}

func validateState(state fixtureState) error {
	u, err := url.Parse(state.URL)
	if err != nil || u.Scheme != "git" || !identifier.MatchString(u.Hostname()) || u.Port() != "9418" ||
		u.Path != "/coverage.git" || u.RawPath != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("source must be an exact private git:// alias:9418/coverage.git URL")
	}
	if state.SchemaVersion != 1 || !identifier.MatchString(state.Alias) || !identifier.MatchString(state.SourceID) ||
		state.Ref != "main" || state.Path != manifestPath || state.Image == "" ||
		strings.ContainsAny(state.Image, "\x00\r\n\t ") || len(state.Image) > 512 {
		return errors.New("invalid fixture identity or image")
	}
	return nil
}

func initialize(ctx context.Context, repo string, state fixtureState) error {
	if err := validateState(state); err != nil {
		return err
	}
	// Exclusive creation; no reset/remove of an existing repository or receipt.
	if err := os.Mkdir(repo, 0o755); err != nil {
		return errors.New("repository path is not free")
	}
	if err := os.Mkdir(filepath.Join(repo, "jobs"), 0o755); err != nil {
		return errors.New("cannot create owned jobs directory")
	}
	image, _ := json.Marshal(state.Image)
	manifest := fmt.Sprintf("apiVersion: v1\nkind: Job\nmetadata:\n  alias: %s\n  cache: false\ntrigger:\n  type: cron\n  configuration:\n    cron: \"0 0 31 2 *\"\nsteps:\n  - name: imported\n    image: %s\n    engine: docker\n    command: [\"sh\", \"-c\", \"echo '##caesium::output {\\\"marker\\\":\\\"initial\\\"}'\"]\n", state.Alias, image)
	if err := os.WriteFile(filepath.Join(repo, manifestPath), []byte(manifest), 0o644); err != nil {
		return errors.New("cannot create initial manifest")
	}
	if err := os.WriteFile(filepath.Join(repo, "jobs", "ignored.txt"), []byte("not a job definition\n"), 0o644); err != nil {
		return errors.New("cannot create ignored fixture file")
	}
	for _, argv := range [][]string{{"init", "--initial-branch=main"}, {"config", "user.name", "Coverage fixture"},
		{"config", "user.email", "coverage@example.invalid"}, {"add", "--", "jobs"}, {"commit", "-m", "Initial owned fixture"}} {
		if _, err := nativeGit(ctx, repo, argv...); err != nil {
			return err
		}
	}
	var err error
	state.InitialCommit, err = nativeGit(ctx, repo, "rev-parse", "HEAD")
	if err != nil || !commitID.MatchString(state.InitialCommit) {
		return errors.New("cannot obtain initial commit")
	}
	state.GitVersion, err = nativeGit(ctx, repo, "--version")
	if err != nil || !strings.HasPrefix(state.GitVersion, "git version ") {
		return errors.New("native Git version is unavailable")
	}
	f, err := os.OpenFile(filepath.Join(filepath.Dir(repo), "state.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return errors.New("fixture state path is not free")
	}
	writeErr := json.NewEncoder(f).Encode(state)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		return errors.New("cannot persist fixture state")
	}
	return nil
}

func readState(path string) (fixtureState, error) {
	var state fixtureState
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16384 {
		return state, errors.New("fixture state must be a bounded regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return state, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil || len(data) > 16384 {
		return state, errors.New("incomplete or oversized state")
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, err
	}
	return state, validateState(state)
}

func validateRepository(ctx context.Context, repo string, state fixtureState) error {
	info, err := os.Lstat(filepath.Join(repo, ".git"))
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !commitID.MatchString(state.InitialCommit) {
		return errors.New("owned Git repository is absent or substituted")
	}
	head, err := nativeGit(ctx, repo, "rev-parse", "HEAD")
	if err != nil || head != state.InitialCommit {
		return errors.New("initial repository identity differs from state")
	}
	branch, err := nativeGit(ctx, repo, "symbolic-ref", "--short", "HEAD")
	if err != nil || branch != state.Ref {
		return errors.New("repository branch differs from state")
	}
	return nil
}

// Discard inherited Git configuration/transport hooks. Commands are argv-only;
// safe.directory applies to the one explicitly owned repository, never '*'.
func gitEnvironment() []string {
	var out []string
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "GIT_") {
			out = append(out, item)
		}
	}
	return append(out, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
}

func gitCommand(ctx context.Context, repo string, args ...string) *exec.Cmd {
	argv := []string{"-c", "safe.directory=" + repo, "-c", "core.hooksPath=/dev/null", "-C", repo}
	cmd := exec.CommandContext(ctx, "git", append(argv, args...)...)
	cmd.Env = gitEnvironment()
	cmd.WaitDelay = time.Second
	return cmd
}

func nativeGit(ctx context.Context, repo string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := gitCommand(ctx, repo, args...)
	var out boundedOutput
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", errors.New("native Git command failed")
	}
	if out.overflow {
		return "", errors.New("native Git output exceeded bound")
	}
	return strings.TrimSpace(string(out.data)), nil
}

type boundedOutput struct {
	data     []byte
	overflow bool
}

func (b *boundedOutput) Write(data []byte) (int, error) {
	if len(b.data)+len(data) > 16384 {
		b.overflow = true
	} else {
		b.data = append(b.data, data...)
	}
	return len(data), nil
}

func readRequest(r io.Reader, gitURL string) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return errors.New("incomplete Git request header")
	}
	n, err := strconv.ParseUint(string(header[:]), 16, 16)
	if err != nil || n < 5 || n > 8192 {
		return errors.New("invalid Git request length")
	}
	data := make([]byte, int(n)-4)
	if _, err := io.ReadFull(r, data); err != nil {
		return errors.New("incomplete Git request")
	}
	u, err := url.Parse(gitURL)
	if err != nil {
		return errors.New("invalid configured Git URL")
	}
	// Both clients' ordinary v0 default-port spellings are supported, with no
	// extra capabilities, service commands, alternate repositories or hosts.
	for _, host := range []string{u.Hostname(), u.Host} {
		if string(data) == "git-upload-pack /coverage.git\x00host="+host+"\x00" {
			return nil
		}
	}
	return errors.New("git request is outside the owned read-only source")
}

func uploadPack(ctx context.Context, conn net.Conn, repo, gitURL string) error {
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	closeOnCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer closeOnCancel()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	if err := readRequest(conn, gitURL); err != nil {
		return err
	}
	if err := conn.SetDeadline(time.Now().Add(60 * time.Second)); err != nil {
		return err
	}
	cmd := gitCommand(ctx, repo, "upload-pack", "--strict", filepath.Join(repo, ".git"))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = conn, conn, io.Discard
	if err := cmd.Run(); err != nil {
		return errors.New("native upload-pack failed")
	}
	return nil
}

func serve(ctx context.Context, listener net.Listener, repo, gitURL string) error {
	ctx, cancel := context.WithCancel(ctx)
	closed := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer closed()
	var joined sync.WaitGroup
	defer func() {
		cancel()
		joined.Wait()
	}()
	slots := make(chan struct{}, 8)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return errors.New("fixture listener failed")
		}
		select {
		case slots <- struct{}{}:
			joined.Add(1)
			go func() {
				defer joined.Done()
				defer func() { <-slots }()
				_ = uploadPack(ctx, conn, repo, gitURL)
			}()
		default:
			_ = conn.Close()
		}
	}
}
