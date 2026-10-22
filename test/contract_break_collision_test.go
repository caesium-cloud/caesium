//go:build integration

package test

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
)

// TestContractBreakNotificationWithholdsCollidingForeignFinding
// (identity-and-access W7-ε): derived edge ids are plain concatenations, so
// marketing's edge P -> "C:/z" on dataset D and finance's edge P -> "C" on
// "z:/D" share an id and derivation merges finance's finding into the
// marketing edge, attributed to marketing's consumer. P drops the key only
// finance's consumer requires, under an allow-breaking acknowledgement: the
// shared destination receives the break naming the key, while marketing's
// destination receives nothing of it.
func (s *IntegrationTestSuite) TestContractBreakNotificationWithholdsCollidingForeignFinding() {
	s.requirePolicyFixture()
	s.skipWithoutHostWebhooks()
	if onAuthLane() {
		s.T().Skip("runs on the no-auth lane, beside TestContractBreakNotificationScopedToNamespace")
	}
	stamp := time.Now().UnixNano()
	producer := fmt.Sprintf("w7e-np-%d", stamp)
	financeConsumer := fmt.Sprintf("w7e-nc-%d", stamp)
	marketingConsumer := financeConsumer + ":/z"
	dataset := fmt.Sprintf("w7e-npub-%d", stamp)
	financeKey := fmt.Sprintf("w7efin_notice_%d", stamp)
	s.cleanupInOrder(marketingConsumer, financeConsumer, producer)
	cleanup := &sharedObjectCleanup{s: s}
	defer cleanup.run()

	shared := func(keys ...string) map[string]any {
		properties := map[string]any{"other": map[string]any{"type": "string"}}
		out := map[string]any{"type": "object", "properties": properties}
		if len(keys) == 0 {
			return out
		}
		required := make([]any, 0, len(keys))
		for _, key := range keys {
			properties[key] = map[string]any{"type": "string"}
			required = append(required, key)
		}
		out["required"] = required
		return out
	}
	producing := func(keys ...string) schema.Definition {
		return s.datasetJob(producer, "marketing", nil, []schema.ProducedDataset{
			{Name: dataset}, {Name: "z:/" + dataset, Schema: shared(keys...)},
		})
	}
	s.applyDefinition(producing(financeKey))
	s.applyDefinition(s.datasetJob(marketingConsumer, "marketing", []schema.ConsumedDataset{{Name: dataset}}, nil))
	s.applyDefinition(s.datasetJob(financeConsumer, "finance",
		[]schema.ConsumedDataset{{Name: "z:/" + dataset, Schema: shared(financeKey)}}, nil))
	mktJob, finJob := s.requireJobByAlias(marketingConsumer), s.requireJobByAlias(financeConsumer)

	captures := map[string]*d4Capture{}
	for _, namespace := range []string{"*", "marketing"} {
		capture := newD4Capture()
		defer capture.Close()
		captures[namespace] = capture
		label := d4Label(namespace)
		channel := s.d4WebhookChannel(s.authAPIKey, namespace, fmt.Sprintf("w7e-collide-%s-%d", label, stamp), capture.URL())
		cleanup.add("/v1/notifications/channels/" + channel)
		policy := s.d4Policy(s.authAPIKey, namespace, fmt.Sprintf("w7e-collide-%s-%d", label, stamp), channel, "contract_break_declared", mktJob.ID, finJob.ID)
		cleanup.add("/v1/notifications/policies/" + policy)
	}

	status, body := s.jobdefRequest("/v1/jobdefs/apply", s.authAPIKey, map[string]any{
		"definitions":    []schema.Definition{producing()},
		"allow_breaking": map[string]string{"dataset": dataset, "reason": "w7e colliding break"},
	})
	s.Require().Equal(http.StatusOK, status, body)

	s.Require().Eventually(func() bool { return len(captures["*"].Snapshot()) >= 1 }, 60*time.Second, 250*time.Millisecond,
		"the shared policy receives the break")
	s.Contains(strings.Join(captures["*"].Snapshot(), "\n"), financeKey, "precondition: the merged finding names finance's key")
	s.Require().Never(func() bool {
		for _, notice := range captures["marketing"].Snapshot() {
			if strings.Contains(notice, financeKey) {
				return true
			}
		}
		return false
	}, 5*time.Second, 250*time.Millisecond, "a finding merged from finance's edge never reaches marketing")
}
