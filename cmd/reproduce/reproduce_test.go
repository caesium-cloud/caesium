package reproduce

import (
	"strings"
	"testing"

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
