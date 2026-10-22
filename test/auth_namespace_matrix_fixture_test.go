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
	JobTrigger string
	Backfill   string
	Victim     *jobSummary // deleted by the DELETE /v1/jobs/:id probe

	// marketing and finance only
	HTTPJob                                    *jobSummary
	HTTPTrigger                                string
	EventJob                                   *jobSummary
	EventTrigger                               string
	EventID                                    string // an ingested event delivered only to EventJob
	FailJob                                    *jobSummary
	FailRun                                    string
	Incident                                   string
	Channel, Policy, Profile                   string
	VictimChannel, VictimPolicy, VictimProfile string
	Dataset                                    string // declared by Job alone
	Hold                                       string // an active hold on Dataset
	RevokeKey                                  string // a key confined to the namespace, revoked by a probe
	RotateKey                                  string
	ListedKey                                  string // a key confined to the namespace, listed by GET /v1/auth/keys
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
	// The moved job: created in finance, run (OldRun), moved to marketing,
	// run again (NewRun).
	Moved          *jobSummary
	MovedDef       schema.Definition
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
// cluster-wide) the probes act on.
func (s *IntegrationTestSuite) matrixKeyVictim(f *nsRoutesFixture, kind string, namespaces ...string) string {
	s.T().Helper()
	args := []string{"--role", "viewer", "--description", "mx victim " + kind}
	for _, ns := range namespaces {
		args = append(args, "--namespace", ns)
	}
	key := s.createAPIKeyCLI(args...)
	f.keys = append(f.keys, key.Meta.ID)
	return key.Meta.ID
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
	for _, n := range fx.ns {
		n.JobTrigger = s.matrixJobTrigger(f, n.Job.ID)
	}
	fx.MovedDef = s.movedJobDefinition(fx.name("moved"), nsFinance)
	fx.Moved = s.matrixApply(f, fx.MovedDef)

	// Runs. Marketing admits one run at a time, so its runs queue.
	for _, n := range fx.ns {
		n.Run1 = s.triggerRun(n.Job.ID)
	}
	fx.OldRun = s.triggerRun(fx.Moved.ID)
	for _, n := range []*matrixNamespace{mkt, fin} {
		if n.FailJob != nil {
			n.FailRun = s.triggerRun(n.FailJob.ID)
		}
	}
	s.Equal(nsFinance, s.awaitRun(fx.Moved.ID, fx.OldRun, runTimeout).Namespace)
	fx.MovedDef.Metadata.Namespace = nsMarketing
	s.applyDefinition(fx.MovedDef)
	fx.NewRun = s.triggerRun(fx.Moved.ID)
	for _, n := range fx.ns {
		s.Require().Equal("succeeded", s.awaitRun(n.Job.ID, n.Run1, runTimeout).Status, n.Name)
		n.Run2 = s.triggerRun(n.Job.ID)
	}
	for _, n := range fx.ns {
		s.Require().Equal("succeeded", s.awaitRun(n.Job.ID, n.Run2, runTimeout).Status, n.Name)
	}
	s.Equal(nsMarketing, s.awaitRun(fx.Moved.ID, fx.NewRun, runTimeout).Namespace)
	for _, n := range []*matrixNamespace{mkt, fin} {
		if n.FailJob != nil {
			n.Incident = s.a8AwaitIncidentForRun(n.FailJob.ID, n.FailRun)
		}
	}

	// Ingested events, each delivered to one namespace's listener only.
	for _, n := range []*matrixNamespace{mkt, fin} {
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
	for _, n := range []*matrixNamespace{mkt, fin} {
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
	for _, n := range []*matrixNamespace{mkt, fin} {
		n.Channel = s.matrixAdminCreate(f, "/v1/notifications/channels", fx.channelBody(n.Name, n.Name+"-ch"))
		n.Policy = s.matrixAdminCreate(f, "/v1/notifications/policies", fx.policyBody(n.Name, n.Channel, n.Name+"-pol"))
		n.Profile = s.matrixAdminCreate(f, "/v1/agentprofiles", fx.profileBody(n.Name, n.Name+"-prof"))
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
	for _, n := range []*matrixNamespace{mkt, fin} {
		n.Dataset = fx.name(n.Name + "-ds")
		declare(n.Job, n.Def.Metadata.Alias, n.Dataset)
		declare(n.Job, n.Def.Metadata.Alias, fx.BothDataset)
		n.Hold = hold(n.Dataset, n.Job)
	}
	fx.BothHold = hold(fx.BothDataset, mkt.Job)
	prefix := "mx" + fx.stamp + "-"
	f.undo = append(f.undo, func() {
		s.catalogExec(f, -1, `DELETE FROM dataset_holds WHERE name LIKE ?`, prefix+"%")
		s.catalogExec(f, -1, `DELETE FROM dataset_declarations WHERE name LIKE ? AND name NOT IN (?, ?)`, prefix+"%", fx.Root, fx.FinDataset)
		s.catalogExec(f, -1, `DELETE FROM dataset_states WHERE name LIKE ?`, prefix+"%")
	})

	// Keys the admin probes revoke, rotate and list.
	for _, n := range []*matrixNamespace{mkt, fin} {
		n.RevokeKey = s.matrixKeyVictim(f, n.Name+" revoke", n.Name)
		n.RotateKey = s.matrixKeyVictim(f, n.Name+" rotate", n.Name)
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
