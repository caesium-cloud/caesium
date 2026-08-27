//go:build integration

package test

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// The infra lane
// ---------------------------------------------------------------------------

const (
	// infraLaneEnv gates every TestInfra* scenario. The podman, helm and
	// kubernetes lanes bring up their own servers without ever running
	// `just build-pack`, so an unguarded scenario would go red there the
	// moment the pack images stopped being present — and those lanes have no
	// required check, so it would go red silently.
	infraLaneEnv = "CAESIUM_INFRA_LANE"

	// packImageTagEnv selects which locally built pack images the scenarios
	// reference. `just integration-up-infra` builds them at the lane's tag.
	packImageTagEnv = "CAESIUM_PACK_IMAGE_TAG"

	// hostProjectRootEnv carries the repo root as the *host* Docker daemon
	// sees it. See hostProjectRoot below for why that is not the same path
	// this process uses.
	hostProjectRootEnv = "CAESIUM_HOST_PROJECT_ROOT"
)

// requireInfraLane skips with an explicit, actionable reason unless the infra
// lane is active.
func (s *IntegrationTestSuite) requireInfraLane() {
	s.T().Helper()
	if strings.TrimSpace(os.Getenv(infraLaneEnv)) != "true" {
		s.T().Skipf(
			"infra lane not enabled: set %s=true and run `just integration-test-infra`, "+
				"which builds the caesiumcloud/{git-source,tf-discover,tf-warm,tf-runner} images this scenario mounts",
			infraLaneEnv,
		)
	}
}

// packImage returns the reference for one pack role image. The tag is
// configurable so the lane can pin the images it just built rather than
// whatever `latest` happens to be on the host.
func (s *IntegrationTestSuite) packImage(role string) string {
	tag := strings.TrimSpace(os.Getenv(packImageTagEnv))
	if tag == "" {
		tag = "latest"
	}
	return fmt.Sprintf("caesiumcloud/%s:%s", role, tag)
}

// hostProjectRoot is the repo root as the Docker daemon sees it.
//
// The integration tests run inside a container with the repo bind-mounted at
// /bld/caesium, but the Caesium server they drive talks to the *host* daemon
// over the shared socket. A `bind` volume source in a job manifest is resolved
// by that daemon, so it has to be a host path — /bld/caesium does not exist
// there. `just integration-test-infra` passes the host repo root in
// CAESIUM_HOST_PROJECT_ROOT; when it is unset (running the tests directly on a
// host) the two paths coincide.
func (s *IntegrationTestSuite) hostProjectRoot() string {
	if root := strings.TrimSpace(os.Getenv(hostProjectRootEnv)); root != "" {
		return root
	}
	return s.projectRoot
}

// ---------------------------------------------------------------------------
// The hermetic infra fixture
// ---------------------------------------------------------------------------

// infraFixture is one materialized copy of pack/testdata/infra: a git
// repository on a path that both this process and the host Docker daemon can
// address, plus an empty workspace directory the pipeline's steps share.
type infraFixture struct {
	s *IntegrationTestSuite

	// root/repo/workspace are this process's view of the fixture.
	root      string
	repo      string
	workspace string

	// hostRoot/hostRepo/hostWorkspace are the same three directories as the
	// Docker daemon sees them. These are the values a job manifest's
	// `volumes[].source.bind` must carry.
	hostRoot      string
	hostRepo      string
	hostWorkspace string
}

// newInfraFixture copies pack/testdata/infra to a fresh directory, commits it
// as a git repository, and registers cleanup.
//
// The copy lives under the repo's own .tmp/ rather than os.TempDir(): the
// directory has to be reachable from the host daemon, and the repo root is the
// one path this container and the host agree on.
func (s *IntegrationTestSuite) newInfraFixture(name string) *infraFixture {
	s.T().Helper()

	rel := filepath.Join(".tmp", "infra-fixtures", fmt.Sprintf("%s-%d", name, time.Now().UnixNano()))
	f := &infraFixture{
		s:             s,
		root:          filepath.Join(s.projectRoot, rel),
		repo:          filepath.Join(s.projectRoot, rel, "repo"),
		workspace:     filepath.Join(s.projectRoot, rel, "workspace"),
		hostRoot:      filepath.Join(s.hostProjectRoot(), rel),
		hostRepo:      filepath.Join(s.hostProjectRoot(), rel, "repo"),
		hostWorkspace: filepath.Join(s.hostProjectRoot(), rel, "workspace"),
	}

	require.NoError(s.T(), os.MkdirAll(f.workspace, 0o755))
	copyTree(s, filepath.Join(s.projectRoot, "pack", "testdata", "infra"), f.repo)

	// The pack images run as an unprivileged user (uid 10001) while this
	// process is root, so the tree has to be world-readable and the workspace
	// world-writable or every role fails on permissions. This is fixture
	// plumbing, not a property of the images.
	relaxPermissions(s, f.root)

	f.gitInit()
	s.T().Cleanup(func() { _ = os.RemoveAll(f.root) })
	return f
}

// HeadCommit returns the fixture repository's current HEAD sha.
func (f *infraFixture) HeadCommit() string {
	f.s.T().Helper()
	return strings.TrimSpace(f.git("rev-parse", "HEAD"))
}

// EditFile rewrites one path inside the fixture repository and commits the
// change, returning the new HEAD. It is how the "edit one stack" and "edit a
// shared module" scenarios move a fingerprint.
func (f *infraFixture) EditFile(relPath, contents string) string {
	f.s.T().Helper()
	target := filepath.Join(f.repo, filepath.FromSlash(relPath))
	require.NoError(f.s.T(), os.MkdirAll(filepath.Dir(target), 0o777))
	require.NoError(f.s.T(), os.WriteFile(target, []byte(contents), 0o666))
	require.NoError(f.s.T(), os.Chmod(target, 0o666))
	f.git("add", "-A")
	f.git("commit", "-m", "edit "+relPath)
	return f.HeadCommit()
}

// ReadFile returns the current contents of a path inside the fixture.
func (f *infraFixture) ReadFile(relPath string) string {
	f.s.T().Helper()
	data, err := os.ReadFile(filepath.Join(f.repo, filepath.FromSlash(relPath)))
	require.NoError(f.s.T(), err)
	return string(data)
}

func (f *infraFixture) gitInit() {
	f.s.T().Helper()
	f.git("init", "--initial-branch=main")
	f.git("add", "-A")
	f.git("commit", "-m", "fixture: hermetic infra repo")
}

// git runs one git command in the fixture repository. Identity and safety
// config are supplied per invocation so the fixture never depends on — or
// mutates — the runner's global git configuration.
func (f *infraFixture) git(args ...string) string {
	f.s.T().Helper()
	full := append([]string{
		"-c", "user.name=Caesium Integration",
		"-c", "user.email=integration@caesium.test",
		"-c", "commit.gpgsign=false",
		"-c", "safe.directory=" + f.repo,
	}, args...)
	cmd := exec.CommandContext(f.s.T().Context(), "git", full...)
	cmd.Dir = f.repo
	out, err := cmd.CombinedOutput()
	require.NoErrorf(f.s.T(), err, "git %v failed: %s", args, string(out))
	return string(out)
}

// copyTree copies src to dst, preserving the directory layout. It is
// deliberately simple: the fixture holds only regular files and directories.
func copyTree(s *IntegrationTestSuite, src, dst string) {
	s.T().Helper()
	require.NoError(s.T(), filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("fixture contains a non-regular file %s", rel)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	}))
}

// relaxPermissions makes every directory traversable and writable and every
// file readable and writable, so the non-root pack containers can both read the
// source tree and write into the workspace.
func relaxPermissions(s *IntegrationTestSuite, root string) {
	s.T().Helper()
	require.NoError(s.T(), filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(path, 0o777)
		}
		return os.Chmod(path, 0o666)
	}))
}

// ---------------------------------------------------------------------------
// Fixture self-test
// ---------------------------------------------------------------------------

// TestInfraFixtureMaterializesAsAGitRepo guards the fixture itself: every later
// infra scenario clones this repository through git-source, so a fixture that
// fails to materialize would surface as an unrelated pipeline failure.
func (s *IntegrationTestSuite) TestInfraFixtureMaterializesAsAGitRepo() {
	s.requireInfraLane()

	f := s.newInfraFixture("selftest")

	for _, rel := range []string{
		"stacks/stacks.yaml",
		"stacks/network/main.tf",
		"stacks/network/.terraform.lock.hcl",
		"stacks/account/main.tf",
		"stacks/app-web/main.tf",
		"stacks/app-web/extra.auto.tfvars.json",
		"modules/vpc/main.tf",
		"modules/tags/main.tf",
		"modules/tags/inner/main.tf",
		"fail-closed/dynamic-source/main.tf",
	} {
		_, err := os.Stat(filepath.Join(f.repo, filepath.FromSlash(rel)))
		s.Require().NoErrorf(err, "fixture is missing %s", rel)
	}

	first := f.HeadCommit()
	s.Require().Len(first, 40, "HEAD should be a full sha")

	second := f.EditFile("stacks/app-web/variables.tf", "variable \"replica_count\" {\n  type    = number\n  default = 3\n}\n")
	s.Require().NotEqual(first, second, "editing a file must produce a new commit")
	s.Require().Contains(f.ReadFile("stacks/app-web/variables.tf"), "default = 3")

	// The host view differs from this process's view only by the repo-root
	// prefix; if that mapping is wrong every bind mount in the lane resolves to
	// a directory the daemon cannot see.
	s.Require().Equal(
		strings.TrimPrefix(f.repo, s.projectRoot),
		strings.TrimPrefix(f.hostRepo, s.hostProjectRoot()),
		"host and container fixture paths must differ only by the repo root",
	)
	s.Require().True(filepath.IsAbs(f.hostRepo), "bind sources must be absolute host paths")

	s.Require().True(
		strings.HasPrefix(s.packImage("git-source"), "caesiumcloud/git-source:"),
		"pack images publish under the caesiumcloud org, got %q", s.packImage("git-source"),
	)
}
