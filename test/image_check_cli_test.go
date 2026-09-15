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
	s.Run("Podman and Kubernetes images are advisory without a Docker probe", func() {
		missingSocket := filepath.Join(s.T().TempDir(), "docker.sock")
		for _, engine := range []string{"podman", "kubernetes"} {
			engine := engine
			s.Run(engine, func() {
				imageRef := "caesium-dx-" + engine + "-only:" + uuid.NewString()
				path := s.writeImageCheckManifest("advisory-"+engine, imageRef, engine)
				stdout, stderr, err := s.runCLIWithEnv([]string{"DOCKER_HOST=unix://" + missingSocket}, "test", "--path", path, "--check-images")
				s.Require().NoError(err, "stdout:\n%s\nstderr:\n%s", stdout, stderr)
				s.Contains(stdout, "ADVISORY  "+imageRef)
				s.Contains(stdout, "no local Docker daemon probe")
				if engine == "podman" {
					s.Contains(stdout, "Podman runtime availability is not checked")
				} else {
					s.Contains(stdout, "Kubernetes target availability is not checked")
				}
				s.NotContains(stderr, "one or more checks failed")
			})
		}
	})

	s.Run("scenario cannot silently skip requested image check", func() {
		stdout, stderr, err := s.runCLISeparate("test", "--scenario", s.T().TempDir(), "--check-images")
		s.Require().Error(err)
		s.Empty(strings.TrimSpace(stdout))
		s.Contains(stderr, "--check-images cannot be combined with --scenario")
	})

	if s.engineType != "docker" {
		return
	}

	present := s.presentDockerImageForCheck()

	s.Run("present local image", func() {
		path := s.writeImageCheckManifest("present", present, "")
		stdout, stderr, err := s.runCLISeparate("test", "--path", path, "--check-images")
		s.Require().NoError(err, "stdout:\n%s\nstderr:\n%s", stdout, stderr)
		s.Contains(stdout, "PASS  "+present+"  (available in local Docker daemon)")
		s.NotContains(stderr, "one or more checks failed", "a successful image check must not report a command failure")
	})

	s.Run("missing local image", func() {
		missing := "caesium-dx-nonexistent-image:" + uuid.NewString()
		path := s.writeImageCheckManifest("missing", missing, "")
		stdout, stderr, err := s.runCLISeparate("test", "--path", path, "--check-images")
		s.Require().Error(err, "an unavailable local image must make --check-images fail")
		s.Contains(stdout, "MISS  "+missing+"  (not found in local Docker daemon)")
		s.NotContains(stdout, "one or more checks failed", "command diagnostics belong on stderr")
		s.Contains(stderr, "one or more checks failed")
	})

	s.Run("unreachable local daemon", func() {
		path := s.writeImageCheckManifest("unreachable", present, "")
		missingSocket := filepath.Join(s.T().TempDir(), "docker.sock")
		stdout, stderr, err := s.runCLIWithEnv([]string{"DOCKER_HOST=unix://" + missingSocket}, "test", "--path", path, "--check-images")
		s.Require().Error(err, "an unreachable Docker daemon must make --check-images fail")
		s.Contains(stdout, "FAIL  "+present+"  (local Docker daemon error:")
		s.NotContains(stdout, "one or more checks failed", "command diagnostics belong on stderr")
		s.Contains(stderr, "one or more checks failed")
	})

	s.Run("Docker and Kubernetes shared image remains strict", func() {
		path := s.writeImageCheckManifest("mixed", present, "docker", "kubernetes")
		missingSocket := filepath.Join(s.T().TempDir(), "docker.sock")
		stdout, stderr, err := s.runCLIWithEnv([]string{"DOCKER_HOST=unix://" + missingSocket}, "test", "--path", path, "--check-images")
		s.Require().Error(err, "a Docker target must remain strict when it shares an image with Kubernetes")
		s.Contains(stdout, "FAIL  "+present+"  (local Docker daemon error:")
		s.Contains(stdout, "Kubernetes target availability is not checked")
		s.NotContains(stdout, "ADVISORY  "+present)
		s.NotContains(stdout, "one or more checks failed")
		s.Contains(stderr, "one or more checks failed")
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

func (s *IntegrationTestSuite) writeImageCheckManifest(suffix, imageRef string, engines ...string) string {
	s.T().Helper()
	dir := s.T().TempDir()
	var steps strings.Builder
	for i, engine := range engines {
		engineLine := ""
		if engine != "" {
			engineLine = "    engine: " + engine + "\n"
		}
		_, _ = fmt.Fprintf(&steps, "  - name: verify-%d\n    image: %s\n%s    command: [\"sh\", \"-c\", \"true\"]\n", i, imageRef, engineLine)
	}
	if len(engines) == 0 {
		steps.WriteString("  - name: verify\n    image: " + imageRef + "\n    command: [\"sh\", \"-c\", \"true\"]\n")
	}
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: image-check-%s
trigger:
  type: cron
  configuration: {cron: "0 2 * * *"}
steps:
%s`, suffix, steps.String())
	path := filepath.Join(dir, "image-check.job.yaml")
	s.Require().NoError(os.WriteFile(path, []byte(manifest), 0o644))
	return path
}
