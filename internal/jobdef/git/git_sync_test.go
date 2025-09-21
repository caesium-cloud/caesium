package git

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	httpauth "github.com/go-git/go-git/v5/plumbing/transport/http"
	sshauth "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/stretchr/testify/suite"
)

type GitSyncSuite struct {
	suite.Suite
}

func TestGitSyncSuite(t *testing.T) {
	suite.Run(t, new(GitSyncSuite))
}

func (s *GitSyncSuite) TestSyncSingleDocument() {
	repoDir := s.initRepo(map[string]string{
		"jobs/sample.yaml": testutil.SampleJob,
	})

	db := testutil.OpenTestDB(s.T())
	s.T().Cleanup(func() { testutil.CloseDB(db) })

	importer := jobdef.NewImporter(db)
	source := Source{URL: repoDir, Ref: "master", Path: "jobs"}

	s.Require().NoError(source.Sync(context.Background(), importer))
	testutil.AssertCount(s.T(), db, &models.Job{}, 1)
	testutil.AssertCount(s.T(), db, &models.Trigger{}, 1)
	testutil.AssertCount(s.T(), db, &models.Atom{}, 3)
	testutil.AssertCount(s.T(), db, &models.Task{}, 3)
}

func (s *GitSyncSuite) TestSyncMultiDocument() {
	multi := testutil.SampleJob + "\n---\n" + strings.Replace(testutil.SampleJob, "csv-to-parquet", "csv-to-parquet-2", 1)
	repoDir := s.initRepo(map[string]string{
		"jobs/multi.yaml": multi,
	})

	db := testutil.OpenTestDB(s.T())
	s.T().Cleanup(func() { testutil.CloseDB(db) })

	importer := jobdef.NewImporter(db)
	source := Source{URL: repoDir, Ref: "master", Path: "jobs"}

	s.Require().NoError(source.Sync(context.Background(), importer))
	testutil.AssertCount(s.T(), db, &models.Job{}, 2)
}

func (s *GitSyncSuite) TestWatchOnce() {
	repoDir := s.initRepo(map[string]string{
		"jobs/sample.yaml": testutil.SampleJob,
	})

	db := testutil.OpenTestDB(s.T())
	s.T().Cleanup(func() { testutil.CloseDB(db) })

	importer := jobdef.NewImporter(db)
	opts := WatchOptions{
		Source: Source{URL: repoDir, Ref: "master", Path: "jobs"},
		Once:   true,
	}

	s.Require().NoError(Watch(context.Background(), importer, opts))
	testutil.AssertCount(s.T(), db, &models.Job{}, 1)
}

func (s *GitSyncSuite) TestWatchDetectsNewCommit() {
	repoDir := s.initRepo(map[string]string{
		"jobs/sample.yaml": testutil.SampleJob,
	})

	db := testutil.OpenTestDB(s.T())
	s.T().Cleanup(func() { testutil.CloseDB(db) })

	importer := jobdef.NewImporter(db)
	opts := WatchOptions{
		Source:   Source{URL: repoDir, Ref: "master", Path: "jobs"},
		Interval: 50 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, importer, opts) }()

	// allow initial sync
	time.Sleep(150 * time.Millisecond)

	// update repo with new alias
	s.commit(repoDir, map[string]string{
		"jobs/sample.yaml": strings.Replace(testutil.SampleJob, "csv-to-parquet", "csv-to-parquet-2", 1),
	})

	time.Sleep(200 * time.Millisecond)
	cancel()

	err := <-done
	if err != nil && !errors.Is(err, context.Canceled) {
		s.FailNow("watch returned error", err)
	}
	testutil.AssertCount(s.T(), db, &models.Job{}, 2)
}

func (s *GitSyncSuite) TestWatchCancel() {
	repoDir := s.initRepo(map[string]string{
		"jobs/sample.yaml": testutil.SampleJob,
	})

	db := testutil.OpenTestDB(s.T())
	s.T().Cleanup(func() { testutil.CloseDB(db) })

	importer := jobdef.NewImporter(db)
	opts := WatchOptions{
		Source:   Source{URL: repoDir, Ref: "master", Path: "jobs"},
		Interval: 50 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, importer, opts) }()

	time.Sleep(120 * time.Millisecond)
	cancel()

	err := <-done
	if err != nil && !errors.Is(err, context.Canceled) {
		s.FailNow("watch returned error", err)
	}
	testutil.AssertCount(s.T(), db, &models.Job{}, 1)
}

func (s *GitSyncSuite) TestSyncRecordsProvenance() {
	repoDir := s.initRepo(map[string]string{
		"jobs/sample.yaml": testutil.SampleJob,
	})

	repo, err := git.PlainOpen(repoDir)
	s.Require().NoError(err)
	ref, err := repo.Head()
	s.Require().NoError(err)
	hash := ref.Hash().String()

	db := testutil.OpenTestDB(s.T())
	s.T().Cleanup(func() { testutil.CloseDB(db) })

	importer := jobdef.NewImporter(db)
	source := Source{URL: repoDir, Ref: "master", Path: "jobs", SourceID: "git-sync"}

	s.Require().NoError(source.Sync(context.Background(), importer))

	var job models.Job
	s.Require().NoError(db.Where("alias = ?", "csv-to-parquet").First(&job).Error)
	s.Equal("git-sync", job.ProvenanceSourceID)
	s.Equal(repoDir, job.ProvenanceRepo)
	s.Equal("master", job.ProvenanceRef)
	s.Equal(hash, job.ProvenanceCommit)
	s.Equal("jobs/sample.yaml", job.ProvenancePath)
}

func (s *GitSyncSuite) TestSyncPathGlobs() {
	repoDir := s.initRepo(map[string]string{
		"jobs/sample.yaml":      testutil.SampleJob,
		"jobs/include.job.yaml": strings.Replace(testutil.SampleJob, "csv-to-parquet", "csv-to-parquet-2", 1),
	})

	db := testutil.OpenTestDB(s.T())
	s.T().Cleanup(func() { testutil.CloseDB(db) })

	importer := jobdef.NewImporter(db)
	source := Source{
		URL:   repoDir,
		Ref:   "master",
		Path:  "jobs",
		Globs: []string{"**/*.job.yaml"},
	}

	s.Require().NoError(source.Sync(context.Background(), importer))

	var jobs []models.Job
	s.Require().NoError(db.Find(&jobs).Error)
	s.Len(jobs, 1)
	s.Equal("csv-to-parquet-2", jobs[0].Alias)
}

func (s *GitSyncSuite) TestCloneOptionsBasicAuthSecrets() {
	resolver := staticResolver{
		"secret://user": "git-user",
		"secret://pass": "top-secret",
	}

	source := Source{
		URL: "https://example.com/repo.git",
		Auth: &BasicAuth{
			UsernameRef: "secret://user",
			PasswordRef: "secret://pass",
		},
		Resolver: resolver,
	}

	opts, cleanup, err := source.cloneOptions(context.Background())
	s.Require().NoError(err)
	s.Require().NotNil(cleanup)
	cleanup()

	auth, ok := opts.Auth.(*httpauth.BasicAuth)
	s.Require().True(ok)
	s.Equal("git-user", auth.Username)
	s.Equal("top-secret", auth.Password)
}

func (s *GitSyncSuite) TestCloneOptionsBasicAuthMissingResolver() {
	source := Source{
		URL:  "https://example.com/repo.git",
		Auth: &BasicAuth{UsernameRef: "secret://missing"},
	}

	_, _, err := source.cloneOptions(context.Background())
	s.Require().Error(err)
	s.Contains(err.Error(), "secret resolver")
}

func (s *GitSyncSuite) TestCloneOptionsSSHWithSecrets() {
	key := mustGeneratePrivateKey(s.T())
	resolver := staticResolver{
		"secret://ssh-user": "git",
		"secret://ssh-key":  key,
		"secret://known":    githubKnownHostEntry(),
	}

	source := Source{
		URL: "git@github.com:caesium/test.git",
		SSH: &SSHAuth{
			UsernameRef:   "secret://ssh-user",
			PrivateKeyRef: "secret://ssh-key",
			KnownHostsRef: "secret://known",
		},
		Resolver: resolver,
	}

	opts, cleanup, err := source.cloneOptions(context.Background())
	s.Require().NoError(err)
	s.Require().NotNil(cleanup)
	defer cleanup()

	pk, ok := opts.Auth.(*sshauth.PublicKeys)
	s.Require().True(ok)
	s.Equal("git", pk.User)
	s.NotNil(pk.Signer)
	s.NotNil(pk.HostKeyCallback)
}

func (s *GitSyncSuite) TestCloneOptionsSSHKnownHostsPath() {
	key := mustGeneratePrivateKey(s.T())
	knownPath := filepath.Join(s.T().TempDir(), "known_hosts")
	s.Require().NoError(os.WriteFile(knownPath, []byte(githubKnownHostEntry()+"\n"), 0o600))

	source := Source{
		URL: "git@github.com:caesium/test.git",
		SSH: &SSHAuth{
			PrivateKey:     key,
			KnownHostsPath: knownPath,
		},
	}

	opts, cleanup, err := source.cloneOptions(context.Background())
	s.Require().NoError(err)
	s.Require().NotNil(cleanup)
	cleanup()

	pk, ok := opts.Auth.(*sshauth.PublicKeys)
	s.Require().True(ok)
	s.NotNil(pk.HostKeyCallback)

	_, statErr := os.Stat(knownPath)
	s.NoError(statErr)
}

func (s *GitSyncSuite) TestCloneOptionsSSHWithoutKnownHosts() {
	source := Source{
		URL: "git@github.com:caesium/test.git",
		SSH: &SSHAuth{
			PrivateKey: mustGeneratePrivateKey(s.T()),
		},
	}

	_, _, err := source.cloneOptions(context.Background())
	s.Require().Error(err)
	s.Contains(err.Error(), "known hosts")
}

func (s *GitSyncSuite) initRepo(files map[string]string) string {
	dir := s.T().TempDir()
	repo, err := git.PlainInit(dir, false)
	s.Require().NoError(err)

	for rel, content := range files {
		path := filepath.Join(dir, rel)
		s.Require().NoError(os.MkdirAll(filepath.Dir(path), 0o755))
		s.Require().NoError(os.WriteFile(path, []byte(content), 0o644))
	}

	wt, err := repo.Worktree()
	s.Require().NoError(err)
	_, err = wt.Add(".")
	s.Require().NoError(err)

	_, err = wt.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	s.Require().NoError(err)

	return dir
}

func (s *GitSyncSuite) commit(repoDir string, files map[string]string) {
	repo, err := git.PlainOpen(repoDir)
	s.Require().NoError(err)

	wt, err := repo.Worktree()
	s.Require().NoError(err)

	for rel, content := range files {
		path := filepath.Join(repoDir, rel)
		s.Require().NoError(os.MkdirAll(filepath.Dir(path), 0o755))
		s.Require().NoError(os.WriteFile(path, []byte(content), 0o644))
		_, err = wt.Add(rel)
		s.Require().NoError(err)
	}

	_, err = wt.Commit("update", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	s.Require().NoError(err)
}

type staticResolver map[string]string

func (sr staticResolver) Resolve(_ context.Context, ref string) (string, error) {
	if val, ok := sr[ref]; ok {
		return val, nil
	}
	return "", fmt.Errorf("secret %q not found", ref)
}

func mustGeneratePrivateKey(tb testing.TB) string {
	tb.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		tb.Fatalf("generate private key: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	blk := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}
	return string(pem.EncodeToMemory(blk))
}

func githubKnownHostEntry() string {
	return "github.com ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQCj7ndNxQowgcQnjshcLrqPEiiphnt+VTTvDP6mHBL9j1aNUkY4Ue1gvwnGLVlOhGeYrnZaMgRK6+PKCUXaDbC7qtbW8gIkhL7aGCsOr/C56SJMy/BCZfxd1nWzAOxSDPgVsmerOBYfNqltV9/hWCqBywINIR+5dIg6JTJ72pcEpEjcYgXkE2YEFXV1JHnsKgbLWNlhScqb2UmyRkQyytRLtL+38TGxkxCflmO+5Z8CSSNY7GidjMIZ7Q4zMjA2n1nGrlTDkzwDCsw+wqFPGQA179cnfGWOWRVruj16z6XyvxvjJwbz0wQZ75XK5tKSb7FNyeIEs4TT4jk+S4dhPeAUC5y+bDYirYgM4GC7uEnztnZyaVWQ7B381AK4Qdrwt51ZqExKbQpTUNn+EjqoTwvqNj4kqx5QUCI0ThS/YkOxJCXmPUWZbhjpCg56i+2aB6CmK2JGhn57K5mj0MNdBXA4/WnwH6XoPWJzK5Nyu2zB3nAZp+S5hpQs+p1vN1/wsjk="
}
