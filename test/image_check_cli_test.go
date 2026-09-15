//go:build integration

package test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/google/uuid"
)

// TestCheckImagesCLIGatesLocalDockerAvailability drives the documented image
// gate through the shipped CLI. It covers a daemon image that exists, an image
// that is absent, and a daemon endpoint that cannot be reached. Stdout carries
// the per-image verdict while Cobra's command failure stays on stderr.
func (s *IntegrationTestSuite) TestCheckImagesCLIGatesLocalDockerAvailability() {
	if s.engineType != "docker" {
		s.T().Skip("--check-images is a local Docker daemon gate; Docker runtime coverage runs in the Docker integration lane")
	}
	present := s.presentDockerImageForCheck()

	s.Run("present local image", func() {
		path := s.writeImageCheckManifest("present", present, "kubernetes")
		stdout, stderr, err := s.runCLISeparate("test", "--path", path, "--check-images")
		s.Require().NoError(err, "stdout:\n%s\nstderr:\n%s", stdout, stderr)
		s.Contains(stdout, "PASS  "+present)
		s.Contains(stdout, "local Docker daemon; Kubernetes target availability is not checked")
		s.NotContains(stderr, "one or more checks failed", "a successful image check must not report a command failure")
	})

	s.Run("missing local image", func() {
		missing := "caesium-dx-nonexistent-image:" + uuid.NewString()
		path := s.writeImageCheckManifest("missing", missing, "")
		stdout, stderr, err := s.runCLISeparate("test", "--path", path, "--check-images")
		s.Require().Error(err, "an unavailable local image must make --check-images fail")
		s.Contains(stdout, "MISS  "+missing+"  (not found in local Docker daemon; local Docker daemon)")
		s.NotContains(stdout, "one or more checks failed", "command diagnostics belong on stderr")
		s.Contains(stderr, "one or more checks failed")
	})

	s.Run("unreachable local daemon", func() {
		path := s.writeImageCheckManifest("unreachable", present, "")
		missingSocket := filepath.Join(s.T().TempDir(), "docker.sock")
		stdout, stderr, err := s.runCLIWithEnv([]string{"DOCKER_HOST=unix://" + missingSocket}, "test", "--path", path, "--check-images")
		s.Require().Error(err, "an unreachable Docker daemon must make --check-images fail")
		s.Contains(stdout, "FAIL  "+present+"  (local Docker daemon; error:")
		s.NotContains(stdout, "one or more checks failed", "command diagnostics belong on stderr")
		s.Contains(stderr, "one or more checks failed")
	})

	s.Run("scenario cannot silently skip requested image check", func() {
		stdout, stderr, err := s.runCLISeparate("test", "--scenario", s.T().TempDir(), "--check-images")
		s.Require().Error(err)
		s.Empty(strings.TrimSpace(stdout))
		s.Contains(stderr, "--check-images requires job definitions and cannot be used with --scenario")
	})
}

func (s *IntegrationTestSuite) presentDockerImageForCheck() string {
	s.T().Helper()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	s.Require().NoError(err)
	defer func() { s.Require().NoError(cli.Close()) }()

	ctx, cancel := context.WithTimeout(s.T().Context(), 10*time.Second)
	defer cancel()
	images, err := cli.ImageList(ctx, image.ListOptions{})
	s.Require().NoError(err)
	for _, candidate := range images {
		for _, tag := range candidate.RepoTags {
			if tag != "" && tag != "<none>:<none>" {
				return tag
			}
		}
	}
	s.T().Fatal("Docker daemon has no tagged images; integration runner image should be present")
	return ""
}

func (s *IntegrationTestSuite) writeImageCheckManifest(suffix, imageRef, engine string) string {
	s.T().Helper()
	dir := s.T().TempDir()
	engineLine := ""
	if engine != "" {
		engineLine = "    engine: " + engine + "\n"
	}
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: image-check-%s
trigger:
  type: cron
  configuration: {cron: "0 2 * * *"}
steps:
  - name: verify
    image: %s
%s    command: ["sh", "-c", "true"]
`, suffix, imageRef, engineLine)
	path := filepath.Join(dir, "image-check.job.yaml")
	s.Require().NoError(os.WriteFile(path, []byte(manifest), 0o644))
	return path
}
