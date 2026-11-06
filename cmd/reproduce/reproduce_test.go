package reproduce

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"syscall"
	"testing"
	"time"

	ireproduce "github.com/caesium-cloud/caesium/internal/reproduce"
	"github.com/caesium-cloud/caesium/pkg/container"
)

func TestValidateModeFlagsConflictsExitTwo(t *testing.T) {
	tests := []struct {
		name      string
		shellMode bool
		diffMode  bool
		dryRun    bool
		jsonMode  bool
		want      string
	}{
		{name: "shell diff", shellMode: true, diffMode: true, want: "--shell cannot be combined with --diff"},
		{name: "shell dry run", shellMode: true, dryRun: true, want: "--shell cannot be combined with --dry-run"},
		{name: "shell json", shellMode: true, jsonMode: true, want: "--shell cannot be combined with --json"},
		{name: "diff dry run", diffMode: true, dryRun: true, want: "--diff cannot be combined with --dry-run"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateModeFlags(tt.shellMode, tt.diffMode, tt.dryRun, tt.jsonMode)
			if err == nil {
				t.Fatal("validateModeFlags() error = nil, want conflict")
			}
			code, ok := ExitCode(err)
			if !ok || code != 2 {
				t.Fatalf("ExitCode(error) = %d, %t; want 2, true", code, ok)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want %q", err.Error(), tt.want)
			}
		})
	}
}

func TestDockerRunShellArgsUsesInteractiveEntrypointAndExactEnv(t *testing.T) {
	args := dockerRunShellArgs(ireproduce.ShellRequest{
		Image:    "alpine:3.23",
		Shell:    ireproduce.DefaultShell,
		Platform: "linux/amd64",
		TTY:      true,
		WorkDir:  "/work",
		Env: map[string]string{
			"B": "2",
			"A": "1",
		},
		Mounts: []container.Mount{{
			Type:     container.MountTypeBind,
			Source:   "/host/data",
			Target:   "/data",
			ReadOnly: true,
		}},
	})
	joined := strings.Join(args, "\x00")

	for _, want := range []string{
		"run\x00--rm\x00-it\x00--entrypoint\x00/bin/sh",
		"--platform\x00linux/amd64",
		"--workdir\x00/work",
		"-e\x00A=1\x00-e\x00B=2",
		"--mount\x00type=bind,source=/host/data,target=/data,readonly",
		"alpine:3.23",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("docker args %q missing %q", joined, want)
		}
	}
}

func TestDockerRunShellArgsWithoutTTYOmitsT(t *testing.T) {
	args := strings.Join(dockerRunShellArgs(ireproduce.ShellRequest{Image: "img:1"}), " ")
	if !strings.HasPrefix(args, "run --rm -i --entrypoint /bin/sh") || strings.Contains(args, "-it") {
		t.Fatalf("args = %q, want non-TTY -i", args)
	}
}

func TestDockerCreateInjectedShellArgs(t *testing.T) {
	args := dockerCreateInjectedShellArgs(ireproduce.ShellRequest{
		Image:   "gcr.io/distroless/static:nonroot",
		WorkDir: "/work",
		TTY:     true,
		Env:     map[string]string{"A": "1"},
		Mounts:  []container.Mount{{Type: container.MountTypeVolume, Source: "v", Target: "/v"}},
	})
	joined := strings.Join(args, "\x00")
	for _, want := range []string{
		"create\x00-i\x00--label\x00caesium.reproduce.shell=\x00-t\x00--entrypoint\x00/.caesium-shell/sh",
		"--workdir\x00/work",
		"-e\x00PATH=/.caesium-shell/bin:",
		"-e\x00A=1",
		"--mount\x00type=volume,source=v,target=/v",
		"gcr.io/distroless/static:nonroot",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args %q missing %q", joined, want)
		}
	}
	if strings.Contains(strings.Join(dockerCreateInjectedShellArgs(ireproduce.ShellRequest{Image: "i"}), " "), " -t") {
		t.Fatal("non-TTY create must not pass -t")
	}
}

func TestRewrapBusyboxPlacesBinaryUnderShellDir(t *testing.T) {
	var src bytes.Buffer
	tw := tar.NewWriter(&src)
	body := []byte("ELFDATA")
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "bin-" + "busy" + "box", Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(body)
	_ = tw.Close()

	var out bytes.Buffer
	if err := rewrapBusybox(&src, &out); err != nil {
		t.Fatalf("rewrapBusybox() error = %v", err)
	}
	tr := tar.NewReader(&out)
	got := map[string]int64{}
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		got[h.Name] = h.Mode
		if h.Name == ".caesium-shell/sh" {
			b, _ := io.ReadAll(tr)
			if string(b) != string(body) {
				t.Fatalf("body = %q", b)
			}
		}
	}
	if _, ok := got[".caesium-shell/bin/ls"]; !ok {
		t.Fatalf("entries = %v, want applet symlinks", got)
	}
	if got[".caesium-shell/sh"] != 0o755 || got[".caesium-shell/"] != 0o755 {
		t.Fatalf("entries = %v, want dir and BusyBox at 0755", got)
	}
}

func TestRewrapBusyboxRejectsSymlink(t *testing.T) {
	var src bytes.Buffer
	tw := tar.NewWriter(&src)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "bin-" + "busy" + "box", Linkname: "/bin/x"})
	_ = tw.Close()
	if err := rewrapBusybox(&src, io.Discard); err == nil {
		t.Fatal("want error for non-regular BusyBox")
	}
}

func TestDockerProbeShellArgsRunsShellCheckWithoutTTY(t *testing.T) {
	args := strings.Join(dockerProbeShellArgs(ireproduce.ShellRequest{Image: "img:1", TTY: true}), " ")
	if !strings.HasPrefix(args, "run --rm -i --entrypoint /bin/sh") || !strings.HasSuffix(args, "img:1 -c true") {
		t.Fatalf("probe args = %q", args)
	}
}

func TestShellContainersCarrySessionLabel(t *testing.T) {
	req := ireproduce.ShellRequest{Image: "img:1", Session: "sess-1", TTY: false}
	for name, args := range map[string][]string{
		"run":    dockerRunShellArgs(req),
		"create": dockerCreateInjectedShellArgs(req),
	} {
		if !strings.Contains(strings.Join(args, "\x00"), "--label\x00caesium.reproduce.shell=sess-1") {
			t.Fatalf("%s args %v missing session label", name, args)
		}
	}
}

func TestDockerCreateIgnoresCancellationButReportsIt(t *testing.T) {
	orig := dockerOutputFn
	defer func() { dockerOutputFn = orig }()
	var sawErr error
	dockerOutputFn = func(ctx context.Context, args ...string) (string, error) {
		sawErr = ctx.Err()
		return "cid-1", nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	id, err := dockerCreate(ctx, "sess", "create", "img")
	if sawErr != nil {
		t.Fatalf("create ran under a cancelled context: %v", sawErr)
	}
	if id != "cid-1" || !errors.Is(err, context.Canceled) {
		t.Fatalf("dockerCreate = %q, %v; want id returned with context.Canceled", id, err)
	}
}

func TestDockerCreateIsBoundedAndSweepsOnStall(t *testing.T) {
	origOut, origTimeout, origSweep := dockerOutputFn, createTimeout, sweepFn
	defer func() { dockerOutputFn, createTimeout, sweepFn = origOut, origTimeout, origSweep }()
	createTimeout = 50 * time.Millisecond
	dockerOutputFn = func(ctx context.Context, args ...string) (string, error) {
		<-ctx.Done() // a stalled daemon: only the bound can release us
		return "", ctx.Err()
	}
	var swept string
	sweepFn = func(ctx context.Context, session string) {
		if ctx.Err() == nil {
			swept = session
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // parent cancelled; create must still be bounded, not hang
	start := time.Now()
	_, err := dockerCreate(ctx, "sess-9", "create", "img")
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("err = %v, want bounded-timeout error", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("dockerCreate took %s, want ~%s", time.Since(start), createTimeout)
	}
	if swept != "sess-9" {
		t.Fatalf("sweep session = %q, want sess-9", swept)
	}
}

func TestDockerCreateReturnsEvenWhenSweepBlocks(t *testing.T) {
	origOut, origTimeout, origSweep, origSweepTimeout := dockerOutputFn, createTimeout, sweepFn, sweepTimeout
	defer func() {
		dockerOutputFn, createTimeout, sweepFn, sweepTimeout = origOut, origTimeout, origSweep, origSweepTimeout
	}()
	createTimeout, sweepTimeout = 50*time.Millisecond, 100*time.Millisecond
	dockerOutputFn = func(ctx context.Context, args ...string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	release := make(chan struct{})
	defer close(release)
	sweepFn = func(context.Context, string) { <-release } // ignores its ctx: a wedged daemon

	start := time.Now()
	if _, err := dockerCreate(context.Background(), "sess", "create", "img"); err == nil {
		t.Fatal("want timeout error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("dockerCreate took %s, want about create+sweep bounds", elapsed)
	}
}

func TestEnsureShellImagePullsOnlyWhenMissing(t *testing.T) {
	orig := dockerOutputFn
	defer func() { dockerOutputFn = orig }()
	var calls []string
	present := false
	dockerOutputFn = func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, args[0])
		if args[0] == "image" && !present {
			return "", errors.New("no such image")
		}
		return "", nil
	}
	req := ireproduce.ShellRequest{ShellImage: "busybox:1.36.1-musl"}
	if err := ensureShellImage(context.Background(), req); err != nil || strings.Join(calls, ",") != "image,pull" {
		t.Fatalf("missing image: err=%v calls=%v", err, calls)
	}
	calls, present = nil, true
	if err := ensureShellImage(context.Background(), req); err != nil || strings.Join(calls, ",") != "image" {
		t.Fatalf("present image: err=%v calls=%v", err, calls)
	}
}

func TestGracefulCommandCancelsWithSIGTERM(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := gracefulCommand(ctx, "sleep", "30")
	if cmd.Cancel == nil || cmd.WaitDelay <= 0 {
		t.Fatalf("Cancel/WaitDelay not wired: %v %v", cmd.Cancel != nil, cmd.WaitDelay)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = cmd.Wait()
	ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
		t.Fatalf("process state = %v, want terminated by SIGTERM", cmd.ProcessState)
	}
}

func TestShellImageFlagDefault(t *testing.T) {
	f := Cmd.Flags().Lookup("shell-image")
	if f == nil || f.DefValue != "busybox:1.36.1-musl" {
		t.Fatalf("shell-image flag = %+v, want default busybox:1.36.1-musl", f)
	}
}
