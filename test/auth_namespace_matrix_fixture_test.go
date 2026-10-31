//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
)

// The namespace matrix's fixture: real objects in default, marketing and
// finance, created through the API as the lane's cluster admin key, plus the
// catalog-seeded preconditions the API cannot produce cheaply (dataset
// declarations shared by two namespaces and active holds). Every identifier
// carries the fixture stamp, so one string names one row in a body.

const (
	nsDefault   = "default"
	nsMarketing = "marketing"
	nsFinance   = "finance"
)

// matrixNamespace is the fixture's objects in one namespace.
type matrixNamespace struct {
	Name       string
	Def        schema.Definition
	Job        *jobSummary // main job: cron never fires, cache on
	Run1       string
	Run2       string
	TaskID     string // the main job's one task (step "ok")
	AtomID     string
	JobTrigger string
	Backfill   string
	Victim     *jobSummary // deleted by the DELETE /v1/jobs/:id probe

	// Every collection names an own row per namespace, so a default caller's
	// foreign-negative checks are never vacuous.
	EventJob                 *jobSummary
	EventTrigger             string
	EventID                  string // an ingested event delivered only to EventJob
	FailJob                  *jobSummary
	FailRun                  string
	Incident                 string
	Channel, Policy, Profile string
	Dataset                  string // declared by Job alone
	Hold                     string // an active hold on Dataset
	ListedKey                string // a key confined to the namespace, listed by GET /v1/auth/keys

	// marketing and finance only
	HTTPJob                                    *jobSummary
	HTTPTrigger                                string
	VictimChannel, VictimPolicy, VictimProfile string
	RevokeKey                                  string // a key confined to the namespace, revoked by a probe
	RotateKey                                  string
}

// namespaceMatrixFixture is everything the rows probe.
type namespaceMatrixFixture struct {
	s     *IntegrationTestSuite
	caps  matrixLaneCaps
	stamp string
	f     *nsRoutesFixture
	ns    map[string]*matrixNamespace

	// Shared ("*") notification and agent objects.
	SharedChannel, SharedPolicy, SharedProfile string
	// Datasets: produced by marketing and finance; produced by nobody.
	BothDataset, OrphanDataset string
	BothHold                   string
	// Lineage: marketing's main job produces Root; finance's consumes it and
	// produces FinDataset; the column names are schema text markers.
	Root, FinDataset, MktColumn, FinColumn string
	// FinConfig is a label on finance's main job: its configuration's marker.
	FinConfig string
	// The moved job: created in finance, run (OldRun), moved to marketing,
	// run again (NewRun).
	Moved          *jobSummary
	MovedDef       schema.Definition
	MovedTaskID    string // its one task, as the old run ran it
	OldRun, NewRun string
	// Cluster-wide victim keys.
	ClusterRevokeKey, ClusterRotateKey, ClusterListedKey string

	created []string // aliases of jobs the probes create, removed on close
	mu      sync.Mutex
}

func (fx *namespaceMatrixFixture) n(name string) *matrixNamespace { return fx.ns[name] }

// adminPrincipal is the lane's bootstrap admin key.
func (fx *namespaceMatrixFixture) adminPrincipal() matrixPrincipal {
	return bearerPrincipal("lane admin", matrixKindAPIKey, fx.s.authAPIKey, "", map[string]models.Role{models.ClusterNamespace: models.RoleAdmin})
}

func (fx *namespaceMatrixFixture) name(kind string) string {
	return fmt.Sprintf("mx%s-%s", fx.stamp, kind)
}

// createdAlias names a job a probe creates and registers it for cleanup.
func (fx *namespaceMatrixFixture) createdAlias(kind string) string {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	alias := fx.name(kind)
	fx.created = append(fx.created, alias)
	return alias
}

func (s *IntegrationTestSuite) matrixApply(f *nsRoutesFixture, def schema.Definition) *jobSummary {
	s.T().Helper()
	s.applyDefinition(def)
	job := s.requireJobByAlias(def.Metadata.Alias)
	f.jobs = append(f.jobs, job.ID)
	return job
}

func (s *IntegrationTestSuite) matrixJobTrigger(f *nsRoutesFixture, jobID string) string {
	s.T().Helper()
	var trigger string
	s.Require().NoError(f.catalogDB.QueryRowContext(s.T().Context(), `SELECT trigger_id FROM jobs WHERE id = ?`, jobID).Scan(&trigger))
	return trigger
}

// adminCreate POSTs body as the lane admin, requires 201 and returns the id;
// the object is deleted again on close.
func (s *IntegrationTestSuite) matrixAdminCreate(f *nsRoutesFixture, path, body string) string {
	s.T().Helper()
	id := decodeID(s, s.requireNSStatus(http.StatusCreated, http.MethodPost, path, s.authAPIKey, body))
	f.undo = append(f.undo, func() {
		status, out := s.nsRequest(http.MethodDelete, path+"/"+id, s.authAPIKey, "", nil)
		s.Contains([]int{http.StatusNoContent, http.StatusNotFound}, status, "cleanup %s/%s: %s", path, id, out)
	})
	return id
}

func (fx *namespaceMatrixFixture) channelBody(namespace, kind string) string {
	return fmt.Sprintf(`{"namespace":%q,"name":%q,"type":"webhook","config":{"url":"https://example.invalid/mx"}}`, namespace, fx.name(kind))
}

func (fx *namespaceMatrixFixture) policyBody(namespace, channel, kind string) string {
	return fmt.Sprintf(`{"namespace":%q,"name":%q,"channel_id":%q,"event_types":["run_failed"]}`, namespace, fx.name(kind), channel)
}

func (fx *namespaceMatrixFixture) profileBody(namespace, kind string) string {
	return fmt.Sprintf(`{"namespace":%q,"name":%q,"image":"busybox:1.36.1"}`, namespace, fx.name(kind))
}

// matrixKeyVictim mints a key (namespaces: confined to them; none:
// cluster-wide) the probes act on. Revoking it again on close is 404 once a
// probe has: a revoked key reads as not found.
func (s *IntegrationTestSuite) matrixKeyVictim(f *nsRoutesFixture, kind string, namespaces ...string) string {
	s.T().Helper()
	args := []string{"--role", "viewer", "--description", "mx victim " + kind}
	for _, ns := range namespaces {
		args = append(args, "--namespace", ns)
	}
	key := s.createAPIKeyCLI(args...)
	id := key.Meta.ID
	f.undo = append(f.undo, func() {
		status, out := s.nsRequest(http.MethodPost, "/v1/auth/keys/"+id+"/revoke", s.authAPIKey, "", nil)
		s.Contains([]int{http.StatusOK, http.StatusNotFound}, status, "cleanup key %s: %s", id, out)
	})
	return id
}

// newNamespaceMatrixFixture creates the fixture. It runs every job it needs
// to completion before returning, so no probe races a fixture run.
func (s *IntegrationTestSuite) newNamespaceMatrixFixture(caps matrixLaneCaps) *namespaceMatrixFixture {
	s.T().Helper()
	fx := &namespaceMatrixFixture{s: s, caps: caps, ns: map[string]*matrixNamespace{}}
	fx.f = s.newNamespacedRoutesFixture("mx")
	fx.stamp = fmt.Sprint(fx.f.stamp)
	f := fx.f
	ok := false
	defer func() {
		if !ok {
			fx.close()
		}
	}()

	fx.Root, fx.FinDataset = fx.name("root"), fx.name("findata")
	fx.MktColumn, fx.FinColumn = "mxmktcol"+fx.stamp, "mxfincol"+fx.stamp
	fx.FinConfig = "mxfincfg" + fx.stamp
	for _, name := range []string{nsDefault, nsMarketing, nsFinance} {
		n := &matrixNamespace{Name: name}
		fx.ns[name] = n
		n.Def = s.movedJobDefinition(fx.name(name), name)
		switch name {
		case nsMarketing:
			n.Def.Steps[0].Datasets = &schema.StepDatasets{Produces: []schema.ProducedDataset{{Name: fx.Root, Schema: stringProperty(fx.MktColumn)}}}
		case nsFinance:
			n.Def.Steps[0].Datasets = &schema.StepDatasets{
				Consumes: []schema.ConsumedDataset{{Name: fx.Root, Schema: stringProperty(fx.MktColumn)}},
				Produces: []schema.ProducedDataset{{Name: fx.FinDataset, Schema: stringProperty(fx.FinColumn)}},
			}
			n.Def.Metadata.Labels = map[string]string{"mx-config": fx.FinConfig}
		}
	}
	// Producer before consumer; the consumer is deleted first (f.jobs is
	// deleted in order, so register finance's main job before marketing's).
	mkt, fin, def := fx.n(nsMarketing), fx.n(nsFinance), fx.n(nsDefault)
	mkt.Job = s.matrixApply(f, mkt.Def)
	fin.Job = s.matrixApply(f, fin.Def)
	f.jobs[len(f.jobs)-2], f.jobs[len(f.jobs)-1] = f.jobs[len(f.jobs)-1], f.jobs[len(f.jobs)-2]
	def.Job = s.matrixApply(f, def.Def)
	def.Victim = s.addNamespacedJob(f, fx.name("default-victim"), nsDefault)
	mkt.Victim, fin.Victim = f.mktJob, f.finJob

	for _, n := range []*matrixNamespace{mkt, fin} {
		n.HTTPJob = s.matrixApply(f, s.namespacedHTTPJobDefinition(fx.name(n.Name+"-http"), n.Name))
		n.HTTPTrigger = s.matrixJobTrigger(f, n.HTTPJob.ID)
	}
	for _, n := range fx.all() {
		evt := s.namespacedJobDefinition(fx.name(n.Name+"-evt"), n.Name)
		evt.Trigger = schema.Trigger{Type: "event", Configuration: map[string]any{
			"events": []any{map[string]any{"type": fx.name(n.Name + "-event"), "source": "mx"}},
		}}
		n.EventJob = s.matrixApply(f, evt)
		n.EventTrigger = s.matrixJobTrigger(f, n.EventJob.ID)
		if caps.Incidents {
			// A failing run opens a real incident (agent remediation only).
			n.FailJob = s.matrixApply(f, s.a8FailingDefinition(fx.name(n.Name+"-fail"), n.Name))
		}
	}
	task := func(jobID string) (id, atomID string) {
		var tasks []struct {
			ID     string `json:"id"`
			AtomID string `json:"atom_id"`
			Name   string `json:"name"`
		}
		s.Require().NoError(json.Unmarshal([]byte(s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/jobs/"+jobID+"/tasks", s.authAPIKey, "")), &tasks))
		s.Require().Len(tasks, 1, jobID)
		return tasks[0].ID, tasks[0].AtomID
	}
	for _, n := range fx.ns {
		n.JobTrigger = s.matrixJobTrigger(f, n.Job.ID)
		n.TaskID, n.AtomID = task(n.Job.ID)
	}
	// replaySafe: a replay across a namespace move always re-executes, which
	// needs the mark on the baseline run.
	fx.MovedDef = s.movedJobDefinition(fx.name("moved"), nsFinance)
	fx.MovedDef.Metadata.ReplaySafe = true
	fx.Moved = s.matrixApply(f, fx.MovedDef)
	fx.MovedTaskID, _ = task(fx.Moved.ID)

	// Runs. Marketing admits one run at a time (quotas.maxConcurrentRuns: 1)
	// and a start over the cap is parked without a run id, so marketing's
	// runs go one after another while default's and finance's overlap them.
	succeed := func(jobID, runID string) {
		s.Require().Equal("succeeded", s.awaitRun(jobID, runID, runTimeout).Status, jobID)
	}
	runMarketing := func(jobID string) string {
		runID := s.triggerRun(jobID)
		s.awaitRun(jobID, runID, runTimeout)
		return runID
	}
	def.Run1, fin.Run1 = s.triggerRun(def.Job.ID), s.triggerRun(fin.Job.ID)
	fx.OldRun = s.triggerRun(fx.Moved.ID)
	for _, n := range []*matrixNamespace{def, fin} {
		if n.FailJob != nil {
			n.FailRun = s.triggerRun(n.FailJob.ID)
		}
	}
	mkt.Run1 = runMarketing(mkt.Job.ID)
	succeed(mkt.Job.ID, mkt.Run1)
	mkt.Run2 = runMarketing(mkt.Job.ID)
	succeed(mkt.Job.ID, mkt.Run2)
	for _, n := range []*matrixNamespace{def, fin} {
		succeed(n.Job.ID, n.Run1)
		n.Run2 = s.triggerRun(n.Job.ID)
	}
	s.Equal(nsFinance, s.awaitRun(fx.Moved.ID, fx.OldRun, runTimeout).Namespace)
	fx.MovedDef.Metadata.Namespace = nsMarketing
	s.applyDefinition(fx.MovedDef)
	fx.NewRun = runMarketing(fx.Moved.ID)
	s.Equal(nsMarketing, s.awaitRun(fx.Moved.ID, fx.NewRun, runTimeout).Namespace)
	if mkt.FailJob != nil {
		mkt.FailRun = runMarketing(mkt.FailJob.ID)
	}
	for _, n := range []*matrixNamespace{def, fin} {
		succeed(n.Job.ID, n.Run2)
	}
	for _, n := range fx.all() {
		if n.FailJob != nil {
			n.Incident = s.a8AwaitIncidentForRun(n.FailJob.ID, n.FailRun)
		}
	}

	// Ingested events, each delivered to one namespace's listener only.
	for _, n := range fx.all() {
		body := fmt.Sprintf(`{"type":%q,"source":"mx","data":{"mx":%q}}`, fx.name(n.Name+"-event"), fx.stamp)
		out := s.requireNSStatus(http.StatusAccepted, http.MethodPost, "/v1/events", s.authAPIKey, body)
		var resp struct {
			EventID     string `json:"event_id"`
			RunsStarted int    `json:"runs_started"`
		}
		s.Require().NoError(json.Unmarshal([]byte(out), &resp), out)
		s.Require().Equal(1, resp.RunsStarted, "the %s event starts its own listener only: %s", n.Name, out)
		n.EventID = resp.EventID
	}
	for _, n := range fx.all() {
		s.awaitJobIdle(n.EventJob.ID)
	}

	// Backfills (the cron never fires, so they schedule nothing).
	now := time.Now().UTC()
	window := fmt.Sprintf(`{"start":%q,"end":%q}`, now.Add(-2*time.Hour).Format(time.RFC3339), now.Add(-time.Hour).Format(time.RFC3339))
	for _, n := range fx.ns {
		n.Backfill = decodeID(s, s.requireNSStatus(http.StatusAccepted, http.MethodPost, "/v1/jobs/"+n.Job.ID+"/backfill", s.authAPIKey, window))
	}

	// Notification and agent objects, plus the shared ("*") ones.
	fx.SharedChannel = s.matrixAdminCreate(f, "/v1/notifications/channels", fx.channelBody("*", "shared-ch"))
	fx.SharedPolicy = s.matrixAdminCreate(f, "/v1/notifications/policies", fx.policyBody("*", fx.SharedChannel, "shared-pol"))
	fx.SharedProfile = s.matrixAdminCreate(f, "/v1/agentprofiles", fx.profileBody("*", "shared-prof"))
	for _, n := range fx.all() {
		n.Channel = s.matrixAdminCreate(f, "/v1/notifications/channels", fx.channelBody(n.Name, n.Name+"-ch"))
		n.Policy = s.matrixAdminCreate(f, "/v1/notifications/policies", fx.policyBody(n.Name, n.Channel, n.Name+"-pol"))
		n.Profile = s.matrixAdminCreate(f, "/v1/agentprofiles", fx.profileBody(n.Name, n.Name+"-prof"))
	}
	for _, n := range fx.pair() {
		n.VictimChannel = s.matrixAdminCreate(f, "/v1/notifications/channels", fx.channelBody(n.Name, n.Name+"-victim-ch"))
		n.VictimPolicy = s.matrixAdminCreate(f, "/v1/notifications/policies", fx.policyBody(n.Name, n.Channel, n.Name+"-victim-pol"))
		n.VictimProfile = s.matrixAdminCreate(f, "/v1/agentprofiles", fx.profileBody(n.Name, n.Name+"-victim-prof"))
	}

	// Datasets (catalog-seeded declarations: a precondition) and holds.
	declare := func(job *jobSummary, alias, dataset string) {
		s.catalogExec(f, 1, `INSERT INTO dataset_declarations (id, job_id, job_alias, name, direction, created_at, updated_at) VALUES (?, ?, ?, ?, 'produces', ?, ?)`,
			uuid.NewString(), job.ID, alias, dataset, now, now)
	}
	hold := func(dataset string, job *jobSummary) string {
		id := uuid.NewString()
		s.catalogExec(f, 1, `INSERT INTO dataset_holds (id, namespace, name, status, active_key, held_by_job_id, opened_at, created_at, updated_at) VALUES (?, '', ?, 'active', ?, ?, ?, ?, ?)`,
			id, dataset, dataset+"/"+id, job.ID, now, now, now)
		return id
	}
	fx.BothDataset, fx.OrphanDataset = fx.name("both-ds"), fx.name("orphan-ds")
	for _, n := range fx.all() {
		n.Dataset = fx.name(n.Name + "-ds")
		declare(n.Job, n.Def.Metadata.Alias, n.Dataset)
		n.Hold = hold(n.Dataset, n.Job)
	}
	for _, n := range fx.pair() {
		declare(n.Job, n.Def.Metadata.Alias, fx.BothDataset)
	}
	fx.BothHold = hold(fx.BothDataset, mkt.Job)
	prefix := "mx" + fx.stamp + "-"
	f.undo = append(f.undo, func() {
		s.catalogExec(f, -1, `DELETE FROM dataset_holds WHERE name LIKE ?`, prefix+"%")
		s.catalogExec(f, -1, `DELETE FROM dataset_declarations WHERE name LIKE ? AND name NOT IN (?, ?)`, prefix+"%", fx.Root, fx.FinDataset)
		s.catalogExec(f, -1, `DELETE FROM dataset_states WHERE name LIKE ?`, prefix+"%")
	})

	// Keys the admin probes revoke, rotate and list.
	for _, n := range fx.pair() {
		n.RevokeKey = s.matrixKeyVictim(f, n.Name+" revoke", n.Name)
		n.RotateKey = s.matrixKeyVictim(f, n.Name+" rotate", n.Name)
	}
	for _, n := range fx.all() {
		n.ListedKey = s.matrixKeyVictim(f, n.Name+" listed", n.Name)
	}
	fx.ClusterRevokeKey = s.matrixKeyVictim(f, "cluster revoke")
	fx.ClusterRotateKey = s.matrixKeyVictim(f, "cluster rotate")
	fx.ClusterListedKey = s.matrixKeyVictim(f, "cluster listed")

	// Lineage: the impact cone of Root reaches finance's consumer once the
	// runs' lineage is persisted.
	s.Require().Eventually(func() bool {
		var impact redactedImpact
		if err := s.tryGetJSON(impactPath(fx.Root), &impact); err != nil {
			return false
		}
		body, _ := json.Marshal(impact)
		return strings.Contains(string(body), fin.Def.Metadata.Alias)
	}, 60*time.Second, 500*time.Millisecond, "lineage never linked %s to the finance consumer", fx.Root)

	ok = true
	return fx
}

// beforeLate withdraws finance's seeded co-declaration of BothDataset before
// the late probes (job definition applies) run: two producers of one
// dataset is a state only the catalog seed creates, and apply refuses every
// definition while it stands ("dataset produced by multiple jobs").
func (fx *namespaceMatrixFixture) beforeLate() {
	fx.awaitNamespacesIdle(nsDefault, nsMarketing, nsFinance)
	fx.s.catalogExec(fx.f, -1, `DELETE FROM dataset_declarations WHERE name = ? AND job_id = ?`, fx.BothDataset, fx.n(nsFinance).Job.ID)
}

// awaitNamespacesIdle waits until the namespaces have no run in flight and
// none queued: the state in which a start is admitted under marketing's quota
// (a retry over the cap is refused 409, not parked) and a definition applies
// (a job with a running run refuses it). Live jobs only, as the queue gauge
// counts: a job deleted with a run still parked by the quota leaves its row
// behind, so an earlier scenario on the lane must not hold this wait open.
func (fx *namespaceMatrixFixture) awaitNamespacesIdle(namespaces ...string) {
	s := fx.s
	s.T().Helper()
	idle := func(namespace string) bool {
		ctx := s.T().Context()
		var running, queued int
		s.Require().NoError(fx.f.catalogDB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM job_runs WHERE namespace = ? AND status IN ('pending', 'queued', 'running')`, namespace).Scan(&running))
		s.Require().NoError(fx.f.catalogDB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM run_queue WHERE job_id IN (SELECT id FROM jobs WHERE namespace = ? AND deleted_at IS NULL)`, namespace).Scan(&queued))
		return running == 0 && queued == 0
	}
	deadline := time.Now().Add(runTimeout)
	for _, namespace := range namespaces {
		for !idle(namespace) {
			s.Require().Truef(time.Now().Before(deadline), "namespace %s still has runs in flight", namespace)
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// settled makes p wait for marketing, the namespace with a run quota, to idle
// before it is admitted.
func (fx *namespaceMatrixFixture) settled(p matrixProbe) matrixProbe {
	p.Before = func() { fx.awaitNamespacesIdle(nsMarketing) }
	return p
}

// close removes every fixture object (and any job a probe created).
func (fx *namespaceMatrixFixture) close() {
	s := fx.s
	for _, alias := range fx.created {
		if job := s.b9Job(alias); job != nil {
			status, body := s.requestWithKey(http.MethodDelete, "/v1/jobs/"+job.ID, s.authAPIKey, nil)
			s.Contains([]int{http.StatusAccepted, http.StatusNotFound}, status, "delete %s: %s", alias, body)
		}
	}
	for _, n := range fx.ns {
		if n.Job != nil {
			s.awaitJobIdle(n.Job.ID)
		}
	}
	s.closeNamespacedRoutesFixture(fx.f)
}
