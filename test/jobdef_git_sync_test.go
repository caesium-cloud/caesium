//go:build integration

package test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

type gitSyncFixtureState struct {
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

type gitSyncJob struct {
	ID                 string `json:"id"`
	Alias              string `json:"alias"`
	Namespace          string `json:"namespace"`
	ProvenanceSourceID string `json:"provenance_source_id"`
	ProvenanceRepo     string `json:"provenance_repo"`
	ProvenanceRef      string `json:"provenance_ref"`
	ProvenanceCommit   string `json:"provenance_commit"`
	ProvenancePath     string `json:"provenance_path"`
}

type gitSyncRun struct {
	ID          string     `json:"id"`
	JobID       string     `json:"job_id"`
	Status      string     `json:"status"`
	CompletedAt *time.Time `json:"completed_at"`
	Tasks       []struct {
		Status      string            `json:"status"`
		CompletedAt *time.Time        `json:"completed_at"`
		Output      map[string]string `json:"output"`
	} `json:"tasks"`
}

// This scenario never calls Watch/Sync/Importer. The production server starts
// its configured watcher; native Git serves and updates an isolated repository,
// and all Caesium effects are observed via public HTTP and the candidate CLI.
func (s *IntegrationTestSuite) TestJobdefGitSyncLocalRepositoryUpdatesAndPrunes() {
	if !envBool("CAESIUM_JOBDEF_GIT_SYNC_LANE") {
		s.maintenanceMissingPrerequisite("the isolated configured GitSync collector lane")
	}
	repo, state := s.gitSyncLane()
	s.Require().Equal(state.InitialCommit, s.gitSyncCommit(repo, "rev-parse", "HEAD"))
	s.Require().Equal("main", s.gitSyncCommit(repo, "symbolic-ref", "--short", "HEAD"))

	var imported gitSyncJob
	s.Require().Eventually(func() bool {
		var jobs []gitSyncJob
		s.gitSyncRead("/v1/jobs?limit=200", http.StatusOK, &jobs)
		for _, job := range jobs {
			if job.Alias == state.Alias {
				imported = job
				return true
			}
		}
		return false
	}, 60*time.Second, 250*time.Millisecond, "configured watcher never imported initial native Git commit")
	s.gitSyncRequireUUID(imported.ID)
	s.gitSyncAwaitDefinition(imported.ID, state, state.InitialCommit, "initial")

	// A job applied independently through the CLI must survive source pruning.
	sentinelAlias := state.Alias + "-sentinel"
	sentinelManifest := fmt.Sprintf("apiVersion: v1\nkind: Job\nmetadata:\n  alias: %s\ntrigger:\n  type: cron\n  configuration:\n    cron: \"0 0 31 2 *\"\nsteps:\n  - name: sentinel\n    image: %q\n    command: [\"echo\", \"sentinel\"]\n", sentinelAlias, state.Image)
	dir := s.writeJobManifest(sentinelManifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	sentinel := s.requireJobByAlias(sentinelAlias)
	s.gitSyncRequireUUID(sentinel.ID)
	var sentinelDetail gitSyncJob
	s.gitSyncRead("/v1/jobs/"+sentinel.ID, http.StatusOK, &sentinelDetail)
	s.Require().Empty(sentinelDetail.ProvenanceSourceID)

	manifest := filepath.Join(repo, state.Path)
	info, err := os.Lstat(manifest)
	s.Require().NoError(err)
	s.Require().True(info.Mode().IsRegular())
	manifestData, err := os.ReadFile(manifest)
	s.Require().NoError(err)
	s.Require().Equal(1, strings.Count(string(manifestData), "initial"))
	s.Require().NoError(os.WriteFile(manifest, []byte(strings.Replace(string(manifestData), "initial", "updated", 1)), 0o644))
	s.gitSyncCommit(repo, "add", "--", state.Path)
	s.gitSyncCommit(repo, "commit", "-m", "Update owned fixture output")
	updated := s.gitSyncCommit(repo, "rev-parse", "HEAD")
	s.Require().NotEqual(state.InitialCommit, updated)
	s.Require().Regexp(`^[0-9a-f]{40}$`, updated)
	s.gitSyncAwaitDefinition(imported.ID, state, updated, "updated")
	stdout, err := s.runCLIStdout("run", "start", "--job-id", imported.ID, "--server", s.caesiumURL)
	s.Require().NoError(err)
	runID := strings.TrimSpace(stdout)
	s.gitSyncRequireUUID(runID)
	var completed gitSyncRun
	s.Require().Eventually(func() bool {
		s.gitSyncRead(fmt.Sprintf("/v1/jobs/%s/runs/%s", imported.ID, runID), http.StatusOK, &completed)
		return completed.Status == "succeeded" || completed.Status == "failed" || completed.Status == "cancelled"
	}, 90*time.Second, 250*time.Millisecond, "imported updated definition never completed its real CLI run")
	s.Require().Equal(runID, completed.ID)
	s.Require().Equal(imported.ID, completed.JobID)
	s.Require().Equal("succeeded", completed.Status)
	s.Require().NotNil(completed.CompletedAt)
	s.Require().False(completed.CompletedAt.IsZero())
	s.Require().Len(completed.Tasks, 1)
	s.Require().Equal("succeeded", completed.Tasks[0].Status)
	s.Require().NotNil(completed.Tasks[0].CompletedAt)
	s.Require().False(completed.Tasks[0].CompletedAt.IsZero())
	s.Require().Equal("updated", completed.Tasks[0].Output["marker"])

	s.Require().NoError(os.Remove(manifest))
	s.gitSyncCommit(repo, "add", "--", state.Path)
	s.gitSyncCommit(repo, "commit", "-m", "Delete owned fixture definition")
	deleted := s.gitSyncCommit(repo, "rev-parse", "HEAD")
	s.Require().Regexp(`^[0-9a-f]{40}$`, deleted)
	s.Require().NotEqual(updated, deleted)
	s.Require().Eventually(func() bool {
		return s.gitSyncRead("/v1/jobs/"+imported.ID, 0, nil) == http.StatusNotFound
	}, 60*time.Second, 250*time.Millisecond, "source-scoped prune did not retire the imported job")
	var remaining []gitSyncJob
	s.gitSyncRead("/v1/jobs?limit=200", http.StatusOK, &remaining)
	for _, job := range remaining {
		s.Require().NotEqual(state.Alias, job.Alias)
	}
	s.gitSyncRead("/v1/jobs/"+sentinel.ID, http.StatusOK, &sentinelDetail)
	s.Require().Equal(sentinel.ID, sentinelDetail.ID)
	s.Require().Equal(sentinelAlias, sentinelDetail.Alias)
	s.Require().Empty(sentinelDetail.ProvenanceSourceID)

	receiptPath := os.Getenv("CAESIUM_JOBDEF_GIT_RECEIPT")
	s.Require().True(filepath.IsAbs(receiptPath))
	receipt := map[string]any{"schema_version": 1, "source": state, "initial_commit": state.InitialCommit,
		"updated_commit": updated, "deleted_commit": deleted, "job_id": imported.ID,
		"sentinel_id": sentinel.ID, "run_id": runID, "run_status": completed.Status,
		"run_completed_at": completed.CompletedAt, "task_completed_at": completed.Tasks[0].CompletedAt,
		"output_marker": completed.Tasks[0].Output["marker"], "imported_job_absent": true, "sentinel_survived": true}
	f, err := os.OpenFile(receiptPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	s.Require().NoError(err, "receipt must have a fresh owned path")
	writeErr := json.NewEncoder(f).Encode(receipt)
	closeErr := f.Close()
	s.Require().NoError(writeErr)
	s.Require().NoError(closeErr)
}

type gitSyncViolation struct {
	Path            string `json:"path"`
	Alias           string `json:"alias"`
	Namespace       string `json:"namespace"`
	StoredNamespace string `json:"stored_namespace,omitempty"`
	Reason          string `json:"reason"`
}

type gitSyncPruneSkip struct {
	Alias     string `json:"alias"`
	Namespace string `json:"namespace"`
}

type gitSyncAudit struct {
	Outcome    string
	ResourceID string
	Metadata   struct {
		URL          string             `json:"url"`
		Ref          string             `json:"ref"`
		Commit       string             `json:"commit"`
		SourceID     string             `json:"source_id"`
		Namespaces   []string           `json:"namespaces"`
		Applied      int                `json:"applied"`
		Violations   []gitSyncViolation `json:"violations"`
		Pruned       int                `json:"pruned"`
		PruneSkipped []gitSyncPruneSkip `json:"prune_skipped"`
		Error        *string            `json:"error"`
	}
}

// The lane's only source is confined to namespaces ["default"]. Native commits
// add, move and delete manifests; a job planted outside the allowlist through
// the candidate CLI's client-asserted provenance stands for a job the source
// owned before it was narrowed. Every sync is observed through its
// system:git-sync audit row (read with the public DB console, since this lane
// runs without auth) and public job reads. Testify runs this after
// TestJobdefGitSyncLocalRepositoryUpdatesAndPrunes, which needs HEAD at the
// initial commit, so this scenario assumes no particular HEAD.
func (s *IntegrationTestSuite) TestJobdefGitSyncNamespaceAllowlistSkipsForeignManifests() {
	if !envBool("CAESIUM_JOBDEF_GIT_SYNC_LANE") {
		s.maintenanceMissingPrerequisite("the isolated configured GitSync collector lane")
	}
	repo, state := s.gitSyncLane()
	s.Require().Equal("main", s.gitSyncCommit(repo, "symbolic-ref", "--short", "HEAD"))

	// The first scenario deletes the fixture's own manifest; if it is still
	// present it is applied with every commit.
	base := 0
	if _, err := os.Lstat(filepath.Join(repo, state.Path)); err == nil {
		base = 1
	}
	witness, mover := state.Alias+"-ns-witness", state.Alias+"-ns-mover"
	foreign, planted := state.Alias+"-ns-foreign", state.Alias+"-ns-planted"
	relPath := func(alias string) string { return "jobs/" + alias + ".job.yaml" }
	manifest := func(alias, namespace, marker string) string {
		meta := "  alias: " + alias + "\n"
		if namespace != "" {
			meta += "  namespace: " + namespace + "\n"
		}
		return fmt.Sprintf("apiVersion: v1\nkind: Job\nmetadata:\n%s  cache: false\ntrigger:\n  type: cron\n  configuration:\n    cron: \"0 0 31 2 *\"\nsteps:\n  - name: allowlist\n    image: %q\n    engine: docker\n    command: [\"echo\", %q]\n", meta, state.Image, marker)
	}
	write := func(alias, namespace, marker string) {
		s.Require().NoError(os.WriteFile(filepath.Join(repo, relPath(alias)), []byte(manifest(alias, namespace, marker)), 0o644))
		s.gitSyncCommit(repo, "add", "--", relPath(alias))
	}
	commit := func(message string) string {
		s.gitSyncCommit(repo, "commit", "-m", message)
		head := s.gitSyncCommit(repo, "rev-parse", "HEAD")
		s.Require().Regexp(`^[0-9a-f]{40}$`, head)
		return head
	}
	requested := func(alias, namespace string) gitSyncViolation {
		return gitSyncViolation{Path: relPath(alias), Alias: alias, Namespace: namespace, Reason: "requested_namespace_not_allowed"}
	}
	jobsByAlias := func() map[string]gitSyncJob {
		var jobs []gitSyncJob
		s.gitSyncRead("/v1/jobs?limit=200", http.StatusOK, &jobs)
		out := make(map[string]gitSyncJob, len(jobs))
		for _, job := range jobs {
			out[job.Alias] = job
		}
		return out
	}
	readJob := func(id string) gitSyncJob {
		var job gitSyncJob
		s.gitSyncRead("/v1/jobs/"+id, http.StatusOK, &job)
		return job
	}

	// Commit 1: two default manifests and one in a namespace the source may
	// not apply.
	write(witness, "", "witness-1")
	write(mover, "", "mover-1")
	write(foreign, "marketing", "foreign-1")
	commit1 := commit("Add allowlist fixture manifests")
	audit := s.gitSyncAwaitAudit(commit1)
	s.Require().Equal("denied", audit.Outcome)
	s.Require().Equal(state.SourceID, audit.ResourceID)
	s.Require().Equal(state.URL, audit.Metadata.URL)
	s.Require().Equal(state.Ref, audit.Metadata.Ref)
	s.Require().Equal(state.SourceID, audit.Metadata.SourceID)
	s.Require().Equal([]string{"default"}, audit.Metadata.Namespaces)
	s.Require().Nil(audit.Metadata.Error)
	s.Require().Equal(base+2, audit.Metadata.Applied)
	s.Require().Zero(audit.Metadata.Pruned)
	s.Require().NotNil(audit.Metadata.PruneSkipped)
	s.Require().Empty(audit.Metadata.PruneSkipped)
	s.Require().ElementsMatch([]gitSyncViolation{requested(foreign, "marketing")}, audit.Metadata.Violations)
	jobs := jobsByAlias()
	s.Require().NotContains(jobs, foreign, "a manifest outside the allowlist must not be applied")
	for _, alias := range []string{witness, mover} {
		s.Require().Contains(jobs, alias)
		s.Require().Equal("default", jobs[alias].Namespace)
		s.Require().Equal(state.SourceID, jobs[alias].ProvenanceSourceID)
		s.Require().Equal(commit1, jobs[alias].ProvenanceCommit)
	}
	witnessID, moverID := jobs[witness].ID, jobs[mover].ID
	s.gitSyncRequireUUID(witnessID)
	s.gitSyncRequireUUID(moverID)

	// Plant a source-owned job outside the allowlist, as a job the source owned
	// before it was narrowed. The watcher acts only on a new HEAD, so no sync
	// runs until commit 2.
	dir := s.writeJobManifest(manifest(planted, "marketing", "planted"))
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL, "--provenance-source-id", state.SourceID)
	plantedID := jobsByAlias()[planted].ID
	s.gitSyncRequireUUID(plantedID)
	plantedJob := readJob(plantedID)
	s.Require().Equal("marketing", plantedJob.Namespace)
	s.Require().Equal(state.SourceID, plantedJob.ProvenanceSourceID)
	s.Require().Empty(plantedJob.ProvenanceCommit)

	// Commit 2: a forbidden move leaves the job active and unchanged, and
	// prune leaves the source's job outside the allowlist untouched.
	write(mover, "marketing", "mover-2")
	write(witness, "", "witness-2")
	commit2 := commit("Move a manifest out of the allowlist")
	audit = s.gitSyncAwaitAudit(commit2)
	s.Require().Equal("denied", audit.Outcome)
	s.Require().Equal(base+1, audit.Metadata.Applied)
	s.Require().Zero(audit.Metadata.Pruned)
	s.Require().ElementsMatch([]gitSyncViolation{requested(foreign, "marketing"), requested(mover, "marketing")}, audit.Metadata.Violations)
	s.Require().Equal([]gitSyncPruneSkip{{Alias: planted, Namespace: "marketing"}}, audit.Metadata.PruneSkipped)
	moverJob := readJob(moverID)
	s.Require().Equal(moverID, moverJob.ID)
	s.Require().Equal("default", moverJob.Namespace)
	s.Require().Equal(commit1, moverJob.ProvenanceCommit)
	s.Require().Equal(commit2, readJob(witnessID).ProvenanceCommit)
	plantedJob = readJob(plantedID)
	s.Require().Equal("marketing", plantedJob.Namespace)
	s.Require().Empty(plantedJob.ProvenanceCommit)
	s.Require().NotContains(jobsByAlias(), foreign)

	// Commit 3: the narrowed source cannot pull the planted job into its
	// allowlist (its stored namespace is outside it).
	write(planted, "", "planted-3")
	commit3 := commit("Claim the planted job for the default namespace")
	audit = s.gitSyncAwaitAudit(commit3)
	s.Require().Equal("denied", audit.Outcome)
	s.Require().Equal(base+1, audit.Metadata.Applied)
	s.Require().Zero(audit.Metadata.Pruned)
	s.Require().ElementsMatch([]gitSyncViolation{requested(foreign, "marketing"), requested(mover, "marketing"),
		{Path: relPath(planted), Alias: planted, Namespace: "default", StoredNamespace: "marketing", Reason: "stored_namespace_not_allowed"}},
		audit.Metadata.Violations)
	s.Require().NotNil(audit.Metadata.PruneSkipped)
	s.Require().Empty(audit.Metadata.PruneSkipped)
	plantedJob = readJob(plantedID)
	s.Require().Equal("marketing", plantedJob.Namespace)
	s.Require().Empty(plantedJob.ProvenanceCommit)
	s.Require().Empty(plantedJob.ProvenancePath)
	s.Require().Equal(commit3, readJob(witnessID).ProvenanceCommit)

	// Commit 4: deleting every manifest prunes only the allowlisted jobs.
	// jobs/ignored.txt keeps the source path present.
	s.gitSyncCommit(repo, "rm", "-q", "--", relPath(witness), relPath(mover), relPath(foreign), relPath(planted))
	commit4 := commit("Delete the allowlist fixture manifests")
	audit = s.gitSyncAwaitAudit(commit4)
	s.Require().Equal("denied", audit.Outcome)
	s.Require().Equal(base, audit.Metadata.Applied)
	s.Require().NotNil(audit.Metadata.Violations)
	s.Require().Empty(audit.Metadata.Violations)
	s.Require().Equal(2, audit.Metadata.Pruned)
	s.Require().Equal([]gitSyncPruneSkip{{Alias: planted, Namespace: "marketing"}}, audit.Metadata.PruneSkipped)
	s.Require().Equal(http.StatusNotFound, s.gitSyncRead("/v1/jobs/"+witnessID, 0, nil))
	s.Require().Equal(http.StatusNotFound, s.gitSyncRead("/v1/jobs/"+moverID, 0, nil))
	plantedJob = readJob(plantedID)
	s.Require().Equal("marketing", plantedJob.Namespace)
	s.Require().Equal(state.SourceID, plantedJob.ProvenanceSourceID)
	s.Require().NotContains(jobsByAlias(), foreign)
}

// gitSyncAwaitAudit waits for the watcher's system:git-sync audit row for
// commit; the row is written after apply and prune, so it marks the end of
// that sync. Each commit must be audited exactly once.
func (s *IntegrationTestSuite) gitSyncAwaitAudit(commit string) gitSyncAudit {
	s.T().Helper()
	body, err := json.Marshal(map[string]any{
		"sql":   "SELECT outcome, resource_id, metadata FROM audit_logs WHERE actor = 'system:git-sync' AND action = 'jobdef.git_sync'",
		"limit": 1000,
	})
	s.Require().NoError(err)
	var found []gitSyncAudit
	s.maintenancePoll(60*time.Second, func() bool {
		status, data := s.maintenanceHTTP(http.MethodPost, "/v1/database/query", body)
		s.Require().Equal(http.StatusOK, status, "database console query: %s", data)
		var resp struct {
			Columns []struct {
				Name string `json:"name"`
			} `json:"columns"`
			Rows      [][]any `json:"rows"`
			Truncated bool    `json:"truncated"`
		}
		s.Require().NoError(json.Unmarshal(data, &resp))
		s.Require().False(resp.Truncated)
		s.Require().Len(resp.Columns, 3)
		found = found[:0]
		for _, row := range resp.Rows {
			s.Require().Len(row, 3)
			var entry gitSyncAudit
			var ok bool
			entry.Outcome, ok = row[0].(string)
			s.Require().True(ok, "outcome column: %#v", row[0])
			entry.ResourceID, ok = row[1].(string)
			s.Require().True(ok, "resource_id column: %#v", row[1])
			raw, ok := row[2].(string)
			if !ok {
				encoded, err := json.Marshal(row[2])
				s.Require().NoError(err)
				raw = string(encoded)
			}
			s.Require().NoError(json.Unmarshal([]byte(raw), &entry.Metadata), "metadata column: %s", raw)
			if entry.Metadata.Commit == commit {
				found = append(found, entry)
			}
		}
		return len(found) > 0
	}, "configured watcher never audited native Git commit "+commit)
	s.Require().Len(found, 1, "each native Git commit is audited exactly once")
	return found[0]
}

// gitSyncLane validates the isolated lane's fixture and its single configured
// source, which is confined to the default namespace, and returns the native
// repository path with the initialized fixture state.
func (s *IntegrationTestSuite) gitSyncLane() (string, gitSyncFixtureState) {
	s.T().Helper()
	s.Require().Equal("docker", s.engineType)
	root := os.Getenv("CAESIUM_JOBDEF_GIT_FIXTURE_ROOT")
	s.Require().True(filepath.IsAbs(root))
	s.Require().Equal(filepath.Clean(root), root)
	real, err := filepath.EvalSymlinks(root)
	s.Require().NoError(err)
	s.Require().Equal(root, real, "fixture root must not traverse a symlink")
	repo := filepath.Join(root, "coverage.git")
	info, err := os.Lstat(repo)
	s.Require().NoError(err)
	s.Require().True(info.IsDir())
	s.Require().Zero(info.Mode() & os.ModeSymlink)
	info, err = os.Lstat(filepath.Join(repo, ".git"))
	s.Require().NoError(err)
	s.Require().True(info.IsDir())
	s.Require().Zero(info.Mode() & os.ModeSymlink)
	statePath := filepath.Join(root, "state.json")
	info, err = os.Lstat(statePath)
	s.Require().NoError(err)
	s.Require().True(info.Mode().IsRegular())
	s.Require().LessOrEqual(info.Size(), int64(16384))
	data, err := os.ReadFile(statePath)
	s.Require().NoError(err)
	var state gitSyncFixtureState
	s.Require().NoError(json.Unmarshal(data, &state))
	s.Require().Equal(1, state.SchemaVersion)
	idPattern := regexp.MustCompile(`^coverage-git-[a-z0-9][a-z0-9-]{0,39}$`)
	s.Require().Regexp(idPattern, state.SourceID)
	s.Require().Equal(state.SourceID, state.Alias)
	s.Require().Equal("main", state.Ref)
	s.Require().Equal("jobs/imported.job.yaml", state.Path)
	s.Require().Regexp(`^[0-9a-f]{40}$`, state.InitialCommit)
	s.Require().True(strings.HasPrefix(state.GitVersion, "git version "))
	gitURL, err := url.Parse(state.URL)
	s.Require().NoError(err)
	s.Require().Equal("git", gitURL.Scheme)
	s.Require().Regexp(`^[a-z0-9][a-z0-9-]{0,62}$`, gitURL.Hostname())
	s.Require().Equal("9418", gitURL.Port())
	s.Require().Equal("/coverage.git", gitURL.Path)
	s.Require().Empty(gitURL.RawPath)
	s.Require().Nil(gitURL.User)
	s.Require().Empty(gitURL.RawQuery)
	s.Require().Empty(gitURL.Fragment)
	s.Require().True(envBool("CAESIUM_JOBDEF_GIT_ENABLED"))
	s.Require().False(envBool("CAESIUM_JOBDEF_GIT_ONCE"))
	interval, err := time.ParseDuration(os.Getenv("CAESIUM_JOBDEF_GIT_INTERVAL"))
	s.Require().NoError(err)
	s.Require().Greater(interval, time.Duration(0))
	s.Require().LessOrEqual(interval, time.Second)
	var sources []struct {
		URL        string   `json:"url"`
		Ref        string   `json:"ref"`
		Path       string   `json:"path"`
		Globs      []string `json:"globs"`
		SourceID   string   `json:"source_id"`
		Interval   string   `json:"interval"`
		Once       *bool    `json:"once"`
		Namespaces []string `json:"namespaces"`
	}
	s.Require().NoError(json.Unmarshal([]byte(os.Getenv("CAESIUM_JOBDEF_GIT_SOURCES")), &sources))
	s.Require().Len(sources, 1, "no ambient Git sources may run in this lane")
	s.Require().Equal(state.URL, sources[0].URL)
	s.Require().Equal(state.Ref, sources[0].Ref)
	s.Require().Equal("jobs", sources[0].Path)
	s.Require().Equal([]string{"**/*.job.yaml"}, sources[0].Globs)
	s.Require().Equal(state.SourceID, sources[0].SourceID)
	s.Require().NotNil(sources[0].Once)
	s.Require().False(*sources[0].Once)
	s.Require().Equal([]string{"default"}, sources[0].Namespaces, "the lane's source is confined to the default namespace")
	sourceInterval, err := time.ParseDuration(sources[0].Interval)
	s.Require().NoError(err)
	s.Require().Greater(sourceInterval, time.Duration(0))
	s.Require().LessOrEqual(sourceInterval, time.Second)
	return repo, state
}

func (s *IntegrationTestSuite) gitSyncRequireUUID(value string) {
	s.T().Helper()
	id, err := uuid.Parse(value)
	s.Require().NoError(err)
	s.Require().NotEqual(uuid.Nil, id)
	s.Require().Equal(id.String(), value)
}

func (s *IntegrationTestSuite) gitSyncCommit(repo string, args ...string) string {
	s.T().Helper()
	ctx, cancel := context.WithTimeout(s.T().Context(), 30*time.Second)
	defer cancel()
	argv := []string{"-c", "safe.directory=" + repo, "-c", "core.hooksPath=/dev/null", "-C", repo}
	cmd := exec.CommandContext(ctx, "git", append(argv, args...)...)
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "GIT_") {
			cmd.Env = append(cmd.Env, item)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	s.Require().NoError(err, "owned native Git command failed")
	s.Require().LessOrEqual(len(out), 16384)
	return strings.TrimSpace(string(out))
}

func (s *IntegrationTestSuite) gitSyncRead(path string, expected int, target any) int {
	s.T().Helper()
	ctx, cancel := context.WithTimeout(s.T().Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.caesiumURL+path, nil)
	s.Require().NoError(err)
	s.authorize(req)
	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024+1))
	s.Require().NoError(err)
	s.Require().LessOrEqual(len(data), 2*1024*1024)
	if expected != 0 {
		s.Require().Equal(expected, resp.StatusCode)
	} else {
		s.Require().Contains([]int{http.StatusOK, http.StatusNotFound}, resp.StatusCode)
	}
	if target != nil {
		s.Require().NoError(json.Unmarshal(data, target))
	}
	return resp.StatusCode
}

func (s *IntegrationTestSuite) gitSyncAwaitDefinition(id string, state gitSyncFixtureState, commit, marker string) {
	s.T().Helper()
	s.Require().Eventually(func() bool {
		var job gitSyncJob
		s.gitSyncRead("/v1/jobs/"+id, http.StatusOK, &job)
		if job.ProvenanceCommit != commit {
			return false
		}
		s.Require().Equal(id, job.ID)
		s.Require().Equal(state.Alias, job.Alias)
		s.Require().Equal(state.SourceID, job.ProvenanceSourceID)
		s.Require().Equal(state.URL, job.ProvenanceRepo)
		s.Require().Equal(state.Ref, job.ProvenanceRef)
		s.Require().Equal(state.Path, job.ProvenancePath)
		var tasks []struct {
			AtomID string `json:"atom_id"`
			Name   string `json:"name"`
		}
		s.gitSyncRead("/v1/jobs/"+id+"/tasks", http.StatusOK, &tasks)
		s.Require().Len(tasks, 1)
		s.Require().Equal("imported", tasks[0].Name)
		s.gitSyncRequireUUID(tasks[0].AtomID)
		var atom struct {
			Image   string `json:"image"`
			Command string `json:"command"`
		}
		s.gitSyncRead("/v1/atoms/"+tasks[0].AtomID, http.StatusOK, &atom)
		s.Require().Equal(state.Image, atom.Image)
		var command []string
		s.Require().NoError(json.Unmarshal([]byte(atom.Command), &command))
		s.Require().Equal([]string{"sh", "-c", fmt.Sprintf("echo '##caesium::output {\"marker\":\"%s\"}'", marker)}, command)
		return true
	}, 60*time.Second, 250*time.Millisecond, "public definition did not match the actual native Git commit")
}
