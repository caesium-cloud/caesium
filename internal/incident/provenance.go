package incident

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/caesium-cloud/caesium/internal/incident/forge"
	"github.com/caesium-cloud/caesium/internal/jobdef/docdiff"
	"github.com/caesium-cloud/caesium/internal/jobdef/yamlpath"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// provenance.go is the Git-PR half of apply_jobdef_patch's provenance router: an
// approved patch for a job that came from Git lands as a pull request against the
// source repository.
//
// A write token is selected from job data, and an operator can set that data
// through POST /v1/jobdefs/apply. So nothing here trusts it on its face. A job
// reaches the forge only when all of these hold:
//
//   - a CAESIUM_GIT_WRITE_CREDENTIALS entry matches it and lists its namespace;
//   - a configured git source has its source id, repository and ref, admits its
//     namespace, and scopes its manifest path;
//   - the file at that path, read from the forge, declares its alias in its
//     namespace, and is the blob it held at the commit the proposal recorded.
//
// The repository owner and name come from the source's configuration; job data
// only has to agree with it. Anything short of that degrades to `escalate` with
// the rendered diff. A job with provenance never takes the direct route: git sync
// would revert the edit, and a forge outage must not become a silent one.
//
// Nothing read from the repository reaches the incident. The file can hold what
// the incident's readers may not read, such as the webhook secret of a trigger
// shared with another namespace, which the confined render withholds from them.
// So the action's result, its reasons and its errors carry the confined render
// and fixed text, and the file's own diff goes only to the pull request, masked,
// whose readers can read the file.

const (
	degradeNoCredential          = "no_write_credential"
	degradeIncompleteProvenance  = "incomplete_provenance"
	degradeUntrustedProvenance   = "untrusted_provenance"
	degradeInvalidProvenance     = "invalid_provenance"
	degradeCredentialUnavailable = "credential_unavailable"
	degradeManifestChanged       = "manifest_changed"
	degradeNoBaseline            = "no_baseline"
	degradeManifestUnsafe        = "manifest_unsafe"
	degradeManifestUnchanged     = "manifest_unchanged"
	degradeForgeError            = "forge_error"
	degradePullRequestMismatch   = "pull_request_mismatch"

	// routeGitPR is the third value of the result's route field.
	routeGitPR = "git_pr"

	// forgeRouteTimeout bounds every forge call of one action together.
	forgeRouteTimeout = time.Minute
	// escalateTimeout bounds the escalation of a degrade, which runs detached so
	// an action cancelled mid-route still reaches a human.
	escalateTimeout = 10 * time.Second

	// maxDiffBytes bounds a rendered diff on every surface that carries one.
	maxDiffBytes         = 30_000
	maxBodyEvidenceBytes = 8_000
	defaultSyncBranch    = "main"
)

// SecretResolver resolves a secret:// reference at use time.
type SecretResolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// ProvenanceConfig is what the router needs from the server's configuration.
type ProvenanceConfig struct {
	GitSyncEnabled bool
	Sources        env.GitSources
	Credentials    env.GitWriteCredentials
	Resolver       SecretResolver
}

// ProvenanceRouter decides and carries out the Git-PR route.
type ProvenanceRouter struct {
	cfg       ProvenanceConfig
	newClient func(forge.Config) (forge.Client, error)
}

// NewProvenanceRouter builds the router over the server's configuration.
func NewProvenanceRouter(cfg ProvenanceConfig) *ProvenanceRouter {
	return &ProvenanceRouter{cfg: cfg, newClient: forge.New}
}

// SetProvenanceRouter wires the Git-PR route. Without one, every job with git
// provenance degrades to escalate.
func (e *Executor) SetProvenanceRouter(router *ProvenanceRouter) {
	e.provenance = router
}

// patchDegrade is why a patch could not take the Git-PR route. It is recorded on
// the action result and carried by the escalation.
type patchDegrade struct {
	Code   string
	Reason string
	// Branch names a pull request branch left on the forge by a route that
	// failed after creating it.
	Branch string
}

func (d *patchDegrade) Error() string { return d.Code + ": " + d.Reason }

// patchPlan is a job's verified route to the forge. repo, base and path are
// settled against server configuration before any token is touched.
type patchPlan struct {
	credential env.GitWriteCredential
	repo       forge.Repo
	base       string
	path       string
	commit     string
	branch     string
	alias      string
	namespace  string
	// baseline is what the proposal recorded, set once it is bound to the plan.
	baseline *patchBaseline
}

// patchBaseline is the provenance a proposal was reviewed against. The proposal
// records it on the action, in the preview, because everything else moves: git
// sync can stamp a newer commit on the job between the proposal and the approval,
// and the definition is a whole document written against this baseline.
type patchBaseline struct {
	SourceID string `json:"source_id,omitempty"`
	Repo     string `json:"repo,omitempty"`
	Ref      string `json:"ref,omitempty"`
	Commit   string `json:"commit,omitempty"`
	Path     string `json:"path,omitempty"`
}

func jobBaseline(job *models.Job) patchBaseline {
	return patchBaseline{
		SourceID: strings.TrimSpace(job.ProvenanceSourceID),
		Repo:     repoIdentity(job.ProvenanceRepo),
		Ref:      strings.TrimSpace(job.ProvenanceRef),
		Commit:   strings.TrimSpace(job.ProvenanceCommit),
		Path:     strings.TrimSpace(job.ProvenancePath),
	}
}

// repoIdentity names a repository without its URL. A job carries the source's URL
// as configured, which can hold inline credentials, and a baseline is stored on
// the action.
func repoIdentity(raw string) string {
	repo, err := forge.ParseRepoURL(raw)
	if err != nil {
		return ""
	}
	return repo.Host + "/" + repo.String()
}

// proposalBaseline reads the baseline the proposal recorded. Only the server
// writes an action's result, so an agent cannot supply one. The preview holds it
// first, and each outcome of the route copies it forward, so a repeat of the route
// is held to the same commit. nil means none was recorded.
func proposalBaseline(action *models.AgentAction) *patchBaseline {
	var recorded struct {
		Baseline *patchBaseline `json:"baseline"`
	}
	if err := json.Unmarshal(action.Result, &recorded); err != nil {
		return nil
	}
	return recorded.Baseline
}

// bindBaseline ties a plan to the proposal: the commit open compares the manifest
// against becomes the one the proposal recorded, not the one the job carries now.
// With no baseline there is nothing to tell a stale proposal from a fresh one, so
// the patch does not reach the forge.
func bindBaseline(plan *patchPlan, job *models.Job, action *models.AgentAction) *patchDegrade {
	baseline := proposalBaseline(action)
	if baseline == nil || !forge.ValidCommit(baseline.Commit) {
		return &patchDegrade{Code: degradeNoBaseline,
			Reason: "the proposal recorded no synced commit, so nothing shows the manifest is still the one it was reviewed against"}
	}
	now := jobBaseline(job)
	if baseline.SourceID != now.SourceID || baseline.Repo != now.Repo ||
		baseline.Ref != now.Ref || baseline.Path != now.Path {
		return &patchDegrade{Code: degradeManifestChanged,
			Reason: "the job's git source or manifest path changed since this patch was proposed"}
	}
	plan.baseline = baseline
	plan.commit = baseline.Commit
	return nil
}

// plan decides the route from the job and server configuration alone: no network
// and no secret, so it can also preview the route for the approver.
func (r *ProvenanceRouter) plan(job *models.Job, namespace string, actionID uuid.UUID) (*patchPlan, *patchDegrade) {
	branch := forge.BranchForAction(actionID)
	if strings.TrimSpace(job.ProvenanceRepo) == "" {
		// Partial provenance still names an external owner, so it stays off the
		// direct route. It names no repository to open a pull request against.
		return nil, &patchDegrade{Code: degradeIncompleteProvenance,
			Reason: "job has authoritative git provenance; a direct apply would be reverted by the next sync"}
	}
	var credential *env.GitWriteCredential
	if r != nil {
		credential = r.credentialFor(job, namespace)
	}
	if credential == nil {
		// One answer for "no entry" and "an entry for another namespace", so a
		// tenant cannot probe which repositories hold a write grant.
		return nil, &patchDegrade{Code: degradeNoCredential,
			Reason: "no CAESIUM_GIT_WRITE_CREDENTIALS entry covers this job's repository in its namespace"}
	}
	source := r.trustedSource(job, namespace)
	if source == nil {
		return nil, &patchDegrade{Code: degradeUntrustedProvenance,
			Reason: "the job's provenance does not match a configured git source that admits its namespace"}
	}
	repo, err := forge.ParseRepoURL(source.URL)
	if err != nil {
		return nil, &patchDegrade{Code: degradeInvalidProvenance, Reason: "the git source's URL does not name a forge repository"}
	}
	base, err := syncBranch(source.Ref)
	if err == nil {
		err = forge.ValidateBranch(base)
	}
	if err != nil {
		return nil, &patchDegrade{Code: degradeInvalidProvenance, Reason: "the git source's ref is not a branch a pull request can target"}
	}
	manifest := strings.TrimSpace(job.ProvenancePath)
	if err := forge.ValidatePath(manifest); err != nil || !sourceCovers(source, manifest) {
		return nil, &patchDegrade{Code: degradeInvalidProvenance, Reason: "the job's manifest path is not a path its git source syncs"}
	}
	commit := strings.TrimSpace(job.ProvenanceCommit)
	if !forge.ValidCommit(commit) {
		return nil, &patchDegrade{Code: degradeInvalidProvenance, Reason: "the job records no synced commit to compare the manifest against"}
	}
	return &patchPlan{
		credential: *credential,
		repo:       repo,
		base:       base,
		path:       manifest,
		commit:     commit,
		branch:     branch,
		alias:      job.Alias,
		namespace:  namespace,
	}, nil
}

// credentialFor returns the first entry whose every set key matches the job and
// that lists its namespace.
func (r *ProvenanceRouter) credentialFor(job *models.Job, namespace string) *env.GitWriteCredential {
	for i := range r.cfg.Credentials {
		credential := &r.cfg.Credentials[i]
		if id := strings.TrimSpace(credential.SourceID); id != "" && id != strings.TrimSpace(job.ProvenanceSourceID) {
			continue
		}
		if repo := strings.TrimSpace(credential.Repo); repo != "" && !sameRepo(repo, job.ProvenanceRepo) {
			continue
		}
		if slices.Contains(credential.Namespaces, namespace) {
			return credential
		}
	}
	return nil
}

// trustedSource returns the configured git source the job's provenance names. A
// job's provenance is client-asserted when it arrived through the REST apply, so
// the source must agree on id, repository and ref, and a namespace-restricted
// source must admit the job. An unnamed source only matches a job with no source
// id, which only git sync can stamp: REST requires one with any provenance.
func (r *ProvenanceRouter) trustedSource(job *models.Job, namespace string) *env.GitSourceConfig {
	if !r.cfg.GitSyncEnabled {
		return nil
	}
	for i := range r.cfg.Sources {
		source := &r.cfg.Sources[i]
		if strings.TrimSpace(source.SourceID) != strings.TrimSpace(job.ProvenanceSourceID) ||
			!sameRepo(source.URL, job.ProvenanceRepo) ||
			strings.TrimSpace(source.Ref) != strings.TrimSpace(job.ProvenanceRef) {
			continue
		}
		if source.Namespaces != nil && !slices.Contains(source.Namespaces, namespace) {
			continue
		}
		return source
	}
	return nil
}

func sameRepo(a, b string) bool {
	left, err := forge.ParseRepoURL(a)
	if err != nil {
		return false
	}
	right, err := forge.ParseRepoURL(b)
	return err == nil && left == right
}

// syncBranch is the branch a git source tracks, as git sync reads its ref.
func syncBranch(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	switch {
	case ref == "":
		return defaultSyncBranch, nil
	case strings.HasPrefix(ref, "refs/heads/"):
		return strings.TrimPrefix(ref, "refs/heads/"), nil
	case strings.HasPrefix(ref, "refs/"):
		return "", errors.New("ref is not a branch")
	default:
		return ref, nil
	}
}

// sourceCovers reports whether git sync reads filePath from this source.
func sourceCovers(source *env.GitSourceConfig, filePath string) bool {
	relative := filePath
	if root := strings.Trim(strings.TrimSpace(source.Path), "/"); root != "" && root != "." {
		prefix := path.Clean(root) + "/"
		if !strings.HasPrefix(filePath, prefix) {
			return false
		}
		relative = strings.TrimPrefix(filePath, prefix)
	}
	if len(source.Globs) == 0 {
		return yamlpath.IsYAML(filePath)
	}
	return slices.ContainsFunc(source.Globs, func(pattern string) bool {
		match, err := doublestar.PathMatch(strings.TrimSpace(pattern), relative)
		return err == nil && match
	})
}

// patchChange is the content of one pull request. The description quotes the diff
// of the document the file really holds, so it is built once that is read, and
// takes the masked form of that diff.
type patchChange struct {
	rendered *renderedManifest
	title    string
	body     func(diff string) string
	message  string
}

// patchPullRequest is a route that ended at a pull request.
type patchPullRequest struct {
	forge.PullRequest
	// Existing is true when an earlier execution had already opened it.
	Existing bool
}

// open turns a plan into a pull request. Every step is safe to repeat: a retry
// finds the pull request or the branch an earlier attempt left and carries on
// from there. A step that cannot complete returns a *patchDegrade; any other
// error is a refusal of the patch itself.
func (r *ProvenanceRouter) open(ctx context.Context, plan *patchPlan, change *patchChange) (*patchPullRequest, error) {
	ctx, cancel := context.WithTimeout(ctx, forgeRouteTimeout)
	defer cancel()

	token, err := r.resolveToken(ctx, plan.credential)
	if err != nil {
		return nil, &patchDegrade{Code: degradeCredentialUnavailable, Reason: "the write credential's token could not be resolved"}
	}
	client, err := r.newClient(forge.Config{
		Kind:       plan.credential.Forge,
		APIBaseURL: plan.credential.APIBaseURL,
		Token:      token,
		Repo:       plan.repo,
	})
	if err != nil {
		return nil, &patchDegrade{Code: degradeInvalidProvenance, Reason: scrubToken(err.Error(), token)}
	}
	fail := func(err error) *patchDegrade {
		degrade := &patchDegrade{Code: degradeForgeError, Reason: scrubToken(forge.Summary(err), token)}
		switch {
		case errors.Is(err, forge.ErrNotFound), errors.Is(err, forge.ErrConflict):
			degrade.Code = degradeManifestChanged
		case errors.Is(err, forge.ErrFileNotEditable):
			degrade.Code = degradeManifestUnsafe
		}
		return degrade
	}
	withBranch := func(degrade *patchDegrade) *patchDegrade {
		degrade.Branch = plan.branch
		return degrade
	}

	// The replacement is spliced from the file at the proposal's commit first, so a
	// pull request already on the branch can be held to it. The new pull request
	// below writes it only once the branch's head still holds that same file.
	synced, err := client.GetFile(ctx, plan.path, plan.commit)
	if err != nil {
		return nil, fail(err)
	}
	spliced, err := spliceManifest(synced.Content, change.rendered, plan.alias, plan.namespace)
	if err != nil {
		if unsafe, ok := errors.AsType[*patchDegrade](err); ok {
			return nil, unsafe
		}
		return nil, err
	}
	updated := spliced.File
	// accept takes a pull request found on the branch only if it is this change:
	// the branch name is in the preview, so anyone who can push to the repository
	// can open one from it, with another edit or against another base.
	accept := func(found *forge.PullRequest) (*patchPullRequest, error) {
		if degrade := verifyPullRequest(ctx, client, plan, found, updated, fail); degrade != nil {
			return nil, withBranch(degrade)
		}
		return &patchPullRequest{PullRequest: *found, Existing: true}, nil
	}

	existing, err := client.FindPullRequest(ctx, plan.branch)
	if err != nil {
		return nil, fail(err)
	}
	if existing != nil {
		return accept(existing)
	}

	head, err := client.BranchHead(ctx, plan.base)
	if err != nil {
		return nil, fail(err)
	}
	current, err := client.GetFile(ctx, plan.path, head)
	if err != nil {
		return nil, fail(err)
	}
	if current.SHA != synced.SHA || !bytes.Equal(current.Content, synced.Content) {
		return nil, &patchDegrade{Code: degradeManifestChanged,
			Reason: fmt.Sprintf("the manifest changed on the branch since commit %s, the one this patch was proposed against; propose it again", plan.commit)}
	}
	outbound := outboundDocumentDiff(spliced.Original, change.rendered.Document)

	created, err := client.CreateBranch(ctx, plan.branch, head)
	if err != nil {
		return nil, fail(err)
	}
	write := true
	if !created {
		onBranch, err := client.GetFile(ctx, plan.path, plan.branch)
		if err != nil {
			return nil, withBranch(fail(err))
		}
		switch {
		case string(onBranch.Content) == string(updated):
			write = false
		case onBranch.SHA != current.SHA:
			return nil, withBranch(&patchDegrade{Code: degradeManifestChanged,
				Reason: "the pull request branch already holds a different change to the manifest"})
		}
	}
	if write {
		if err := client.PutFile(ctx, forge.PutFile{
			Path:    plan.path,
			Branch:  plan.branch,
			Message: change.message,
			Content: updated,
			SHA:     current.SHA,
		}); err != nil {
			return nil, withBranch(fail(err))
		}
	}

	opened, err := client.OpenPullRequest(ctx, forge.NewPullRequest{
		Title: change.title,
		Body:  change.body(outbound),
		Head:  plan.branch,
		Base:  plan.base,
	})
	if errors.Is(err, forge.ErrConflict) {
		found, findErr := client.FindPullRequest(ctx, plan.branch)
		if findErr != nil {
			return nil, withBranch(fail(findErr))
		}
		if found != nil {
			return accept(found)
		}
	}
	if err != nil {
		degrade := fail(err)
		degrade.Code = degradeForgeError
		return nil, withBranch(degrade)
	}
	return &patchPullRequest{PullRequest: *opened}, nil
}

// verifyPullRequest holds a pull request found on the route's branch to the
// approved change: it targets the plan's base, and the manifest at its head is
// byte for byte the replacement the route would write. Anything else is not the
// change the human approved, so it degrades with fixed text and is never recorded
// as this action's pull request.
func verifyPullRequest(ctx context.Context, client forge.Client, plan *patchPlan, found *forge.PullRequest, updated []byte, fail func(error) *patchDegrade) *patchDegrade {
	if found.Base != plan.base {
		return &patchDegrade{Code: degradePullRequestMismatch,
			Reason: fmt.Sprintf("a pull request from branch %s already exists and does not target %s; it is not the approved change", plan.branch, plan.base)}
	}
	mismatch := &patchDegrade{Code: degradePullRequestMismatch,
		Reason: fmt.Sprintf("a pull request from branch %s already exists and its manifest is not the approved change", plan.branch)}
	if !forge.ValidCommit(found.HeadSHA) {
		return mismatch
	}
	onHead, err := client.GetFile(ctx, plan.path, found.HeadSHA)
	switch {
	case errors.Is(err, forge.ErrNotFound), errors.Is(err, forge.ErrFileNotEditable):
		return mismatch
	case err != nil:
		return fail(err)
	case !bytes.Equal(onHead.Content, updated):
		return mismatch
	}
	return nil
}

// resolveToken reads the token at use time, as git sync reads its credentials.
func (r *ProvenanceRouter) resolveToken(ctx context.Context, credential env.GitWriteCredential) (string, error) {
	if credential.Token != "" {
		return credential.Token, nil
	}
	if r.cfg.Resolver == nil {
		return "", errors.New("no secret resolver")
	}
	token, err := r.cfg.Resolver.Resolve(ctx, strings.TrimSpace(credential.TokenRef))
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(token) == "" {
		return "", errors.New("empty token")
	}
	return token, nil
}

func scrubToken(text, token string) string {
	if token == "" {
		return text
	}
	return strings.ReplaceAll(text, token, Redacted)
}

// dispatchGitSyncedPatch handles an approved patch of a job with git provenance:
// a pull request when the job can prove its route, otherwise an escalation with
// the rendered diff. It never applies to the database.
func (e *Executor) dispatchGitSyncedPatch(ctx context.Context, inc *models.Incident, action *models.AgentAction, job *models.Job, params ActionParams) (map[string]any, error) {
	namespace := IncidentNamespace(inc)
	rendered, err := e.ops.ApplyJobdefPatch(ctx, job.ID, namespace, params.Definition, true)
	if err != nil {
		return nil, err
	}
	diff := newPatchDiff(job.Alias, rendered, params.Definition)
	manifest, renderErr := renderManifest(params.Definition)
	plan, degrade := e.provenance.plan(job, namespace, action.ID)
	if degrade == nil && renderErr != nil {
		degrade = &patchDegrade{Code: degradeManifestUnsafe, Reason: "the proposed definition does not survive the manifest parser: " + renderErr.Error()}
	}
	if degrade == nil {
		degrade = bindBaseline(plan, job, action)
	}
	if degrade == nil {
		pr, err := e.provenance.open(ctx, plan, &patchChange{
			rendered: manifest,
			title:    patchTitle(job.Alias, inc.ID),
			body: func(text string) string {
				return patchBody(inc, action.ID, job.Alias, plan, text)
			},
			message: patchCommitMessage(job.Alias, inc.ID, action.ID),
		})
		if err == nil {
			return patchPullRequestResult(job, plan, pr, diff.forPullRequest(pr.URL)), nil
		}
		routed, ok := errors.AsType[*patchDegrade](err)
		if !ok {
			return nil, err
		}
		degrade = routed
	}
	return e.escalatePatch(ctx, inc, job, action, params, diff, degrade)
}

func patchPullRequestResult(job *models.Job, plan *patchPlan, pr *patchPullRequest, diff patchDiff) map[string]any {
	out := map[string]any{
		"job_id":    job.ID.String(),
		"job_alias": job.Alias,
		"route":     routeGitPR,
		"applied":   false,
		"pr_url":    pr.URL,
		"pr_number": pr.Number,
		"pr_branch": plan.branch,
		"pr_state":  pr.State,
		"repo":      plan.repo.String(),
		"base":      plan.base,
		"path":      plan.path,
		"baseline":  plan.baseline,
		"diff":      diff,
	}
	if pr.Existing {
		out["pr_existing"] = true
	}
	return out
}

// escalatePatch raises the degrade to a human with the rendered diff. It runs
// detached from ctx: a route cancelled part-way still owes that human the truth,
// and the escalation is local.
//
// The action row keeps the diff as the incident's readers may read it. The
// escalation goes further, to the event stream, the server log and whatever
// channels a policy names, so it carries the masked form.
func (e *Executor) escalatePatch(ctx context.Context, inc *models.Incident, job *models.Job, action *models.AgentAction, params ActionParams, diff patchDiff, degrade *patchDegrade) (map[string]any, error) {
	log.Warn("incident: jobdef patch degraded to escalate",
		"incident_id", inc.ID, "job_alias", job.Alias, "degrade", degrade.Code)
	summary := params.Summary
	if summary == "" {
		summary = fmt.Sprintf("approved jobdef patch for git-synced job %q cannot be applied directly; apply it in the source repository", job.Alias)
	}
	escalateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), escalateTimeout)
	defer cancel()
	routed, err := e.ops.Escalate(escalateCtx, inc.ID, params.Channel, summary+"\n"+degrade.Reason+"\n"+diff.outbound)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"job_id":    job.ID.String(),
		"job_alias": job.Alias,
		"route":     routeEscalate,
		"applied":   false,
		"routed":    routed,
		"degrade":   degrade.Code,
		"reason":    degrade.Reason,
		"diff":      diff,
	}
	if degrade.Branch != "" {
		out["pr_branch"] = degrade.Branch
	}
	if baseline := proposalBaseline(action); baseline != nil {
		out["baseline"] = baseline
	}
	return out, nil
}

// recordPatchPreview shows the approver what approving will do. A dispatch
// re-decides everything, so the preview is read-only and best-effort: it calls
// no forge and uses no token, and a proposal the dispatch would refuse records
// none, because the refusal is the answer. It runs the dispatch's own boundary
// checks first, since at this point nothing else has confined the proposal.
//
// The result's `diff` is a string, the shape the Console's approval card renders.
// It is not scrubbed: its readers are the incident's, who can read the proposal
// on the same row and the job's manifest. The agent gets none of it
// (cmd/start withoutPreview), and a redaction here could hide the edit a human is
// deciding on. For a job with provenance the preview also records the baseline
// the dispatch holds the file to, and the diff compares the server's copy of the
// job: it uses no token, so it cannot read the file.
func (e *Executor) recordPatchPreview(ctx context.Context, inc *models.Incident, action *models.AgentAction, params ActionParams) {
	if e.ops == nil || len(params.Definition) == 0 || !authModeActive() {
		return
	}
	if err := e.verifyActionBoundary(ctx, inc, params); err != nil {
		return
	}
	runID, _ := resolveRunID(inc, params)
	if err := verifyNamespaceBoundary(ctx, e.store.DB(), inc, runID); err != nil {
		return
	}
	job, err := e.loadPatchJob(ctx, inc, params)
	if err != nil {
		return
	}
	namespace := IncidentNamespace(inc)
	diff, err := e.ops.ApplyJobdefPatch(ctx, job.ID, namespace, params.Definition, true)
	if err != nil {
		return
	}

	preview := map[string]any{"preview": true, "job_alias": job.Alias}
	var headline string
	if !gitSynced(job) {
		preview["route"] = routeDirect
		headline = "route: direct apply to the server"
	} else {
		baseline := jobBaseline(job)
		preview["baseline"] = baseline
		plan, degrade := e.provenance.plan(job, namespace, action.ID)
		if degrade != nil {
			preview["route"] = routeEscalate
			preview["degrade"] = degrade.Code
			preview["reason"] = degrade.Reason
			headline = "route: escalate with this diff, no change is made (" + degrade.Reason + ")"
		} else {
			preview["route"] = routeGitPR
			preview["repo"] = plan.repo.String()
			preview["base"] = plan.base
			preview["path"] = plan.path
			preview["pr_branch"] = plan.branch
			headline = fmt.Sprintf("route: pull request against %s (%s, %s); escalates instead if the manifest cannot be patched safely or has changed since commit %s",
				plan.repo, plan.base, plan.path, baseline.Commit)
		}
	}
	text := patchDiffText(diff, params.Definition)
	if text == noPatchChanges && gitSynced(job) {
		text = noServerChanges
	}
	preview["diff"] = boundText(headline+"\n\n"+text, maxDiffBytes)

	encoded := encodeJSON(preview)
	if err := e.store.DB().WithContext(ctx).Model(&models.AgentAction{}).
		Where("id = ? AND status = ?", action.ID, models.AgentActionStatusProposed).
		Update("result", encoded).Error; err != nil {
		log.Warn("incident: could not record the patch preview", "action_id", action.ID, "error", err)
		return
	}
	action.Result = encoded
}

// loadPatchJob reads the patch's target and refuses what no route may carry.
func (e *Executor) loadPatchJob(ctx context.Context, inc *models.Incident, params ActionParams) (*models.Job, error) {
	jobID := resolveJobID(inc, params)
	var job models.Job
	if err := e.store.DB().WithContext(ctx).First(&job, "id = ?", jobID).Error; err != nil {
		return nil, fmt.Errorf("incident: load job for jobdef patch: %w", err)
	}
	// The remediation compared (and quoted) below belongs to the job's owner. The
	// caller's namespace check ran on an earlier read, so a move committed since
	// then is caught on THIS row before its remediation is inspected: another
	// namespace's policy never reaches the incident's timeline
	// (identity-and-access A8).
	if models.NamespaceOrDefault(job.Namespace) != IncidentNamespace(inc) {
		return nil, fmt.Errorf("%w: job %s", ErrNamespaceMoved, job.ID)
	}
	// Refuse a self-modifying patch BEFORE the provenance router, so no route can
	// carry a policy edit.
	if err := refuseRemediationEdit(job.Remediation, params.Definition); err != nil {
		return nil, err
	}
	return &job, nil
}

const (
	noPatchChanges = "no changes: the proposed definition is identical to the current one"
	// noServerChanges is noPatchChanges for a job a file owns. The preview compares
	// the server's copy and the route compares the file, which can differ.
	noServerChanges = "no changes to the server's copy of this job; the route compares the manifest file when it runs"
)

// patchDiff is the `diff` of an executed patch's result. Its text is bounded.
type patchDiff struct {
	Alias string `json:"alias"`
	// Empty says the proposed definition is identical to the current one.
	Empty bool   `json:"empty"`
	Diff  string `json:"diff"`
	// outbound is the form for readers outside the incident. It stays off the
	// result, which has Diff.
	outbound string
}

func newPatchDiff(alias string, rendered, definition json.RawMessage) patchDiff {
	text := patchDiffText(rendered, definition)
	return patchDiff{
		Alias:    alias,
		Empty:    text == noPatchChanges,
		Diff:     boundText(text, maxDiffBytes),
		outbound: outboundPatchDiff(rendered, definition, text),
	}
}

// forPullRequest is the diff a pull request's result records: the confined render
// of the server's copy, never the file's (see the note at the top of this file).
// The route refuses a file that already matches the proposal, so a pull request
// is always a change, even when the server's copy shows none: a field the server
// does not persist, or a file that moved ahead of it. Then the result says where
// the change is.
func (d patchDiff) forPullRequest(url string) patchDiff {
	if d.Empty {
		d.Empty = false
		d.Diff = "the server's copy of this job matches the proposal; the change to the manifest is in the pull request: " + url
	}
	return d
}

// outboundPatchDiff is the render's masked diff. With none, it falls back as the
// complete text does, to the proposal, masked.
func outboundPatchDiff(rendered, definition json.RawMessage, text string) string {
	if text == noPatchChanges {
		return noPatchChanges
	}
	var parsed struct {
		Outbound string `json:"outbound_diff"`
	}
	if err := json.Unmarshal(rendered, &parsed); err == nil && parsed.Outbound != "" {
		return boundedSection(parsed.Outbound, maxDiffBytes)
	}
	return outboundProposalText(proposalDocument(definition))
}

// patchDiffText is the readable form of ActionOps.ApplyJobdefPatch's result. It
// says "no changes" only on the render's own word that the definitions are
// identical. When there is no diff to show, it shows the whole proposal.
func patchDiffText(diff, definition json.RawMessage) string {
	var rendered struct {
		Empty bool   `json:"empty"`
		Diff  string `json:"diff"`
	}
	if err := json.Unmarshal(diff, &rendered); err == nil {
		switch {
		case rendered.Diff != "":
			return rendered.Diff
		case rendered.Empty:
			return noPatchChanges
		}
	}
	return fullProposalText(definition)
}

// outboundDocumentDiff is the masked diff of two job documents, for a pull request
// description, falling back to the masked proposal.
func outboundDocumentDiff(current, proposed []byte) string {
	text, err := docdiff.MaskedDocuments(current, proposed)
	switch {
	case err != nil:
		return outboundProposalText(proposed)
	case text == "":
		return noPatchChanges
	}
	return boundedSection(text, maxDiffBytes)
}

const noDiffHeading = "no diff could be produced; the complete proposed definition follows\n\n"

// fullProposalText stands in for a diff that could not be produced: the reader
// cannot see what changes, so they see all of what would be written.
func fullProposalText(definition json.RawMessage) string {
	return noDiffHeading + string(proposalDocument(definition))
}

// outboundProposalText is fullProposalText for readers outside the incident. A
// document that cannot be masked is withheld.
func outboundProposalText(document []byte) string {
	masked, err := docdiff.MaskedDocument(document)
	if err != nil {
		return "no diff could be produced; the proposed definition is on the incident's action"
	}
	return boundedSection(noDiffHeading+masked, maxDiffBytes)
}

// proposalDocument is the proposal as YAML, or as it arrived if it will not decode.
func proposalDocument(definition json.RawMessage) []byte {
	var proposed schema.Definition
	if err := json.Unmarshal(definition, &proposed); err == nil {
		if document, err := yaml.Marshal(&proposed); err == nil {
			return document
		}
	}
	return definition
}

var unsafeForgeText = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// forgeLabel reduces a job value to characters that cannot mention, link or
// format, for the title and commit subject, which render outside any code block.
func forgeLabel(value string) string {
	return strings.Trim(unsafeForgeText.ReplaceAllString(value, "-"), "-")
}

func patchTitle(alias string, incidentID uuid.UUID) string {
	return fmt.Sprintf("Patch job %s for incident %s", forgeLabel(alias), incidentID.String()[:8])
}

func patchCommitMessage(alias string, incidentID, actionID uuid.UUID) string {
	return fmt.Sprintf("Patch job %s\n\nApproved action %s on incident %s.", forgeLabel(alias), actionID, incidentID)
}

// patchBody renders the pull request description. Job and data values are
// untrusted, so they appear only inside fenced code blocks, where a forge renders
// no Markdown, mention or link. Only ids generated here sit outside one.
func patchBody(inc *models.Incident, actionID uuid.UUID, alias string, plan *patchPlan, diff string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Caesium opened this pull request for an approved `apply_jobdef_patch` action.\n\n")
	fmt.Fprintf(&b, "- Incident: `%s`\n- Action: `%s`\n\n", inc.ID, actionID)
	b.WriteString("Merging it lets git sync apply the change. Comments inside the patched job's document are not preserved.\n\n")
	b.WriteString("### Job\n\n")
	b.WriteString(fenced(fmt.Sprintf("alias: %s\nnamespace: %s\nmanifest: %s\nbaseline commit: %s", alias, plan.namespace, plan.path, plan.commit)))
	b.WriteString("\n\n### Violated assertion\n\n")
	b.WriteString(fenced(boundedSection(patchEvidence(inc), maxBodyEvidenceBytes)))
	b.WriteString("\n\n### Rendered diff\n\n")
	b.WriteString("The job's document in the file against its replacement, as data, with keys sorted. Credential values are masked; equal masks mean equal values.\n\n")
	b.WriteString(fenced(diff))
	b.WriteString("\n")
	return b.String()
}

// patchEvidence is the violated assertion with its observed value, bound and
// baseline, taken from the hold's verdicts; other incidents name their class.
func patchEvidence(inc *models.Incident) string {
	if FailureClass(inc.Class) != ClassDataQualityHold {
		return fmt.Sprintf("incident class: %s\ntask: %s\nno dataset assertion is attached to this incident", inc.Class, inc.TaskName)
	}
	evidence := decodeHoldEvidence(inc)
	encoded, err := json.MarshalIndent(map[string]any{
		"dataset_namespace": evidence.DatasetNamespace,
		"dataset":           evidence.Dataset,
		"reason":            evidence.Reason,
		"violations":        evidence.Violations,
	}, "", "  ")
	if err != nil {
		return "the hold's evidence could not be rendered"
	}
	return string(encoded)
}

// boundedSection scrubs and bounds one untrusted section. The pull request goes
// to a repository whose readers are not the incident's, so credential-shaped
// strings stay behind.
func boundedSection(text string, limit int) string {
	return boundText(NewScrubber(nil).Scrub(text), limit)
}

// boundText cuts text at limit bytes and says so.
func boundText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return strings.ToValidUTF8(text[:limit], "") + "\n... truncated; the incident holds the full text"
}

// fenced wraps text in a code fence longer than any backtick run inside it, so
// the text cannot close the fence.
func fenced(text string) string {
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + "\n" + text + "\n" + fence
}
