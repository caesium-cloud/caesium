package testutil

import (
	"context"
	"os/exec"
)

// RunGit executes a fixture command with deterministic identity and signing.
func RunGit(ctx context.Context, dir string, args ...string) (string, error) {
	full := append([]string{
		"-c", "user.name=Pack Test",
		"-c", "user.email=reagent@caesium.test",
		"-c", "commit.gpgsign=false",
		"-c", "safe.directory=" + dir,
	}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
