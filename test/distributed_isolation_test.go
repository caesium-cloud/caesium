//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/pkg/container"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
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
