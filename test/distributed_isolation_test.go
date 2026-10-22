//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/pkg/container"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"gopkg.in/yaml.v3"
)

// isolationLaneEnv marks the isolation-context lane (just
// integration-test-isolation, scripts/integration-test-isolation.sh): an owner
// serving the policy fixture whose worker withdraws isolation_v1, a capable
// worker serving NO policy file, and another worker with isolation_v1 withdrawn.
const isolationLaneEnv = "CAESIUM_ISOLATION_LANE"

// isolationDefinition is a Docker job of steps sequential steps in namespace;
// env becomes each step's environment. With want set, a step fails unless
// $TOKEN arrived resolved to exactly want.
func isolationDefinition(alias, namespace string, env map[string]string, steps int, want string) schema.Definition {
	def := schema.Definition{
		APIVersion: "v1",
		Kind:       "Job",
		Metadata:   schema.Metadata{Alias: alias, Namespace: namespace},
		Trigger:    schema.Trigger{Type: "cron", Configuration: map[string]any{"cron": "0 0 31 2 *"}},
	}
	for i := range steps {
		def.Steps = append(def.Steps, schema.Step{
			Name:    fmt.Sprintf("step-%d", i),
			Image:   "alpine:3.23",
			Command: isolationCommand(want),
			Spec:    container.Spec{Env: env},
		})
	}
	return def
}

func isolationCommand(want string) []string {
	if want == "" {
		return []string{"sh", "-c", "echo ok"}
	}
	return []string{"sh", "-c", fmt.Sprintf(`[ "$TOKEN" = %q ] && echo token-resolved`, want)}
}

func (s *IntegrationTestSuite) applyIsolationJob(def schema.Definition) *jobSummary {
	s.T().Helper()
	dir := s.writeSecretScopeManifest(def)
	_, err := s.runCLIStdout("job", "apply", "--path", dir, "--server", s.caesiumURL)
	s.Require().NoError(err, "apply %s", def.Metadata.Alias)
	return s.requireJobByAlias(def.Metadata.Alias)
}

// TestDistributedIsolationContextReachesCapableWorker drives identity-and-access
// C2 (design §10.1) through a live three-node cluster:
//
//   - a task under marketing's secret rules, allowed and denied, and a task in
//     the Kubernetes-mapped `isolated` namespace each land on the capable
//     worker, which serves no policy file — so the allow, the deny (of a
//     variable that worker could read) and the recorded Kubernetes target can
//     only have come from the owner's isolation context;
//   - the worker without isolation_v1 is a live, used peer (it runs default
//     namespace tasks) yet never receives an isolated task, and the owner
//     counts every skip in caesium_dispatch_incompatible_worker_total.
func (s *IntegrationTestSuite) TestDistributedIsolationContextReachesCapableWorker() {
	if strings.TrimSpace(os.Getenv(isolationLaneEnv)) != "1" {
		s.T().Skipf("%s runs only on the isolation-context lane (just integration-test-isolation)", s.T().Name())
	}
	capable := strings.TrimSpace(os.Getenv("CAESIUM_ISOLATION_CAPABLE_NODE"))
	incapable := strings.TrimSpace(os.Getenv("CAESIUM_ISOLATION_INCAPABLE_NODE"))
	owner := strings.TrimSpace(os.Getenv("CAESIUM_ISOLATION_OWNER_NODE"))
	s.Require().NotEmpty(capable)
	s.Require().NotEmpty(incapable)
	s.Require().NotEmpty(owner)
	suffix := time.Now().UnixNano()
	const metric = "caesium_dispatch_incompatible_worker_total"

	// The incapable worker is a live peer: default-namespace work reaches it.
	finance := s.applyIsolationJob(isolationDefinition(fmt.Sprintf("c2-finance-%d", suffix), "finance", nil, 3, ""))
	usedIncapable := false
	for attempt := 0; attempt < 8 && !usedIncapable; attempt++ {
		r := s.awaitRun(finance.ID, s.triggerRun(finance.ID), runTimeout)
		s.Require().Equal("succeeded", r.Status, "%+v", r.Tasks)
		for _, task := range r.Tasks {
			usedIncapable = usedIncapable || task.ClaimedBy == incapable
		}
	}
	s.Require().True(usedIncapable, "the worker without isolation_v1 runs default-namespace tasks")

	before := s.scrapeCounter(metric, nil)

	// Allowed by marketing's env/MARKETING_* rule, on a worker with no rules.
	allowed := s.applyIsolationJob(isolationDefinition(fmt.Sprintf("c2-marketing-allowed-%d", suffix), "marketing",
		map[string]string{"TOKEN": "secret://env/MARKETING_IT_TOKEN"}, 1, "it-marketing-token"))
	allowedRun := s.awaitRun(allowed.ID, s.triggerRun(allowed.ID), runTimeout)
	s.Require().Equal("succeeded", allowedRun.Status, "%+v", allowedRun.Tasks)
	s.Require().Len(allowedRun.Tasks, 1)
	s.Equal(capable, allowedRun.Tasks[0].ClaimedBy, "an isolated task runs on the capable worker")

	// Denied: the capable worker could resolve ISOLATION_IT_DENIED_TOKEN and
	// serves no rules of its own, so only the owner's context denies it.
	denied := s.applyIsolationJob(isolationDefinition(fmt.Sprintf("c2-marketing-denied-%d", suffix), "marketing",
		map[string]string{"TOKEN": "secret://env/ISOLATION_IT_DENIED_TOKEN"}, 1, "it-denied-token"))
	deniedRun := s.awaitRun(denied.ID, s.triggerRun(denied.ID), runTimeout)
	s.Require().Equal("failed", deniedRun.Status)
	s.Require().Len(deniedRun.Tasks, 1)
	s.Equal(capable, deniedRun.Tasks[0].ClaimedBy)
	s.Contains(deniedRun.Tasks[0].Error, `is not allowed by namespace "marketing" secret rules`)
	s.NotContains(deniedRun.Tasks[0].Error, "secret://env/ISOLATION_IT_DENIED_TOKEN")
	s.Empty(deniedRun.Tasks[0].RuntimeID, "a denied task never starts a container")

	// The Kubernetes-mapped namespace: the capable worker has no policy, so the
	// recorded target can only be the owner's mapping.
	isolated := s.applyIsolationJob(isolationDefinition(fmt.Sprintf("c2-isolated-%d", suffix), "isolated", nil, 1, ""))
	isolatedRun := s.awaitRun(isolated.ID, s.triggerRun(isolated.ID), runTimeout)
	s.Require().Equal("succeeded", isolatedRun.Status, "%+v", isolatedRun.Tasks)
	s.Require().Len(isolatedRun.Tasks, 1)
	s.Equal(capable, isolatedRun.Tasks[0].ClaimedBy)
	var descriptor struct {
		Descriptor json.RawMessage `json:"descriptor"`
	}
	s.fetchDatasetOperatorJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s/tasks/step-0/descriptor", isolated.ID, isolatedRun.ID), &descriptor)
	var recorded struct {
		Runtime struct {
			KubernetesTarget *struct {
				Namespace          string `json:"namespace"`
				ServiceAccountName string `json:"serviceAccountName"`
			} `json:"kubernetesTarget"`
		} `json:"runtime"`
	}
	s.Require().NoError(json.Unmarshal(descriptor.Descriptor, &recorded))
	s.Require().NotNil(recorded.Runtime.KubernetesTarget, "the owner's mapping is recorded: %s", descriptor.Descriptor)
	s.Equal("caesium-isolated", recorded.Runtime.KubernetesTarget.Namespace)
	s.Equal("caesium-isolated-runner", recorded.Runtime.KubernetesTarget.ServiceAccountName)

	// Isolated tasks skipped the owner's and the incapable worker's
	// capability-less workers; neither ever ran one.
	for _, r := range []*runResponse{allowedRun, deniedRun, isolatedRun} {
		for _, task := range r.Tasks {
			s.NotEqual(owner, task.ClaimedBy)
			s.NotEqual(incapable, task.ClaimedBy)
		}
	}
	s.Greater(s.scrapeCounter(metric, nil), before, "every skipped incapable peer is counted")
}

// TestDistributedIsolationReloadFencesPullLaneRows drives the isolation fence
// (design §10.1) through the isolation lane's pull-only node D: D serves a
// writable copy of the policy fixture, runs no run owner (its runs are claimed
// through the pull lane, never pushed) and no worker. A run started on D under
// `finance: {}` registers its rows unmarked; a reload on D that gives finance a
// secret rule while the second step is still pending must keep that step from
// every worker in the cluster — none can honour the new isolation context
// (A and C withdrew isolation_v1, B serves no policy file) — although each of
// them pulled it in the control run.
func (s *IntegrationTestSuite) TestDistributedIsolationReloadFencesPullLaneRows() {
	if strings.TrimSpace(os.Getenv(isolationLaneEnv)) != "1" {
		s.T().Skipf("%s runs only on the isolation-context lane (just integration-test-isolation)", s.T().Name())
	}
	pullURL := strings.TrimSpace(os.Getenv("CAESIUM_ISOLATION_PULL_URL"))
	path := strings.TrimSpace(os.Getenv("CAESIUM_ISOLATION_PULL_POLICY_PATH"))
	s.Require().NotEmpty(pullURL)
	s.Require().NotEmpty(path)
	original, err := os.ReadFile(path)
	s.Require().NoError(err)
	// The lane runs without auth, so D has no reload API: its poller
	// (CAESIUM_AUTH_POLICY_RELOAD_INTERVAL=500ms) picks the file up, and D's
	// lint shows which revision it serves — a finance reference outside
	// env/FINANCE_* is refused only under the isolating one.
	const probeRef = "secret://env/C2_FENCE_PROBE"
	servesIsolating := func() bool {
		for _, msg := range s.lintOnNode(pullURL, "finance", probeRef) {
			if strings.Contains(msg, `is not allowed by namespace "finance" secret rules`) {
				return true
			}
		}
		return false
	}
	s.T().Cleanup(func() {
		s.restoreAuthPolicy(path, original)
		deadline := time.Now().Add(authPolicyPollWait)
		for servesIsolating() {
			if time.Now().After(deadline) {
				s.T().Errorf("node D did not return to the original policy revision")
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
	})
	s.Require().False(servesIsolating(), "node D starts from the fixture, where finance has no rules")

	def := isolationDefinition(fmt.Sprintf("c2-fence-%d", time.Now().UnixNano()), "finance", nil, 2, "")
	def.Steps[0].Command = []string{"sh", "-c", "sleep 10 && echo ok"}
	job := s.applyIsolationJob(def)
	triggerOnPullNode := func() string {
		resp, err := s.doJSONRequest(http.MethodPost, fmt.Sprintf("%s/v1/jobs/%s/run", pullURL, job.ID), nil)
		s.Require().NoError(err)
		defer func() { _ = resp.Body.Close() }()
		s.Require().Equal(http.StatusAccepted, resp.StatusCode)
		var started runResponse
		s.Require().NoError(json.NewDecoder(resp.Body).Decode(&started))
		s.Require().NotEmpty(started.ID)
		return started.ID
	}
	countStatus := func(r runResponse, status string) int {
		n := 0
		for _, task := range r.Tasks {
			if task.Status == status {
				n++
			}
		}
		return n
	}

	// Control: under the unchanged policy the pull lane runs both steps.
	control := s.awaitRun(job.ID, triggerOnPullNode(), runTimeout)
	s.Require().Equal("succeeded", control.Status, "%+v", control.Tasks)

	// Reload while the first step runs and the second is registered, pending.
	runID := triggerOnPullNode()
	s.waitFor(runTimeout, "the first step is claimed while the second waits for it", func() bool {
		r := s.fetchRun(job.ID, runID)
		return len(r.Tasks) == 2 && countStatus(r, "running") == 1 && countStatus(r, "pending") == 1
	})
	p, err := policy.Parse(original)
	s.Require().NoError(err)
	finance := p.Namespaces["finance"]
	finance.Secrets = &policy.SecretRules{Allow: []string{"env/FINANCE_*"}}
	p.Namespaces["finance"] = finance
	isolated, err := yaml.Marshal(p)
	s.Require().NoError(err)
	s.writeAuthPolicy(path, isolated)
	s.waitFor(authPolicyPollWait, "node D reloads the isolating revision", servesIsolating)
	s.Require().Equal(1, countStatus(s.fetchRun(job.ID, runID), "pending"), "the reload landed while the second step was pending")

	s.waitFor(runTimeout, "the claimed first step completes", func() bool {
		return countStatus(s.fetchRun(job.ID, runID), "succeeded") >= 1
	})
	// Every worker pulled the unmarked step within a poll in the control run
	// (500ms interval); the fenced one is pulled by none.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r := s.fetchRun(job.ID, runID)
		s.Require().Equal(1, countStatus(r, "pending"), "the fenced step is never claimed: %+v", r.Tasks)
		for _, task := range r.Tasks {
			if task.Status == "pending" {
				s.Empty(task.ClaimedBy)
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// waitFor polls cond on the test goroutine (it may assert) until it holds,
// failing the test after timeout.
func (s *IntegrationTestSuite) waitFor(timeout time.Duration, what string, cond func() bool) {
	s.T().Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			s.T().Fatalf("timed out after %s waiting until %s", timeout, what)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// lintOnNode posts one definition in namespace that reads ref to baseURL's
// POST /v1/jobdefs/lint and returns its error messages: that node's own policy
// decides them.
func (s *IntegrationTestSuite) lintOnNode(baseURL, namespace, ref string) []string {
	s.T().Helper()
	body := fmt.Sprintf(`{"definitions":[{"apiVersion":"v1","kind":"Job","metadata":{"alias":"c2-fence-probe","namespace":%q},`+
		`"trigger":{"type":"cron","configuration":{"cron":"0 0 31 2 *"}},`+
		`"steps":[{"name":"use","image":"alpine:3.23","env":{"TOKEN":%q}}]}]}`, namespace, ref)
	resp, err := s.doJSONRequest(http.MethodPost, baseURL+"/v1/jobdefs/lint", strings.NewReader(body))
	s.Require().NoError(err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, resp.StatusCode, "%s", raw)
	var doc secretScopeLintDoc
	s.Require().NoError(json.Unmarshal(raw, &doc), "%s", raw)
	messages := make([]string, 0, len(doc.Errors))
	for _, e := range doc.Errors {
		messages = append(messages, e.Message)
	}
	return messages
}
