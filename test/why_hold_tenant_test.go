//go:build integration

package test

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// holdConsumerIn is holdConsumerManifest in a tenancy namespace.
func (s *IntegrationTestSuite) holdConsumerIn(namespace, alias, dataset string) *jobSummary {
	s.T().Helper()
	manifest := strings.Replace(holdConsumerManifest(alias, dataset, ""),
		"  cache: false", "  namespace: "+namespace+"\n  cache: false", 1)
	return s.applyAssertionsJob(alias, manifest)
}

// TestHoldWhyProvenanceIsConfinedToReadableNamespaces reads the explanation of
// a hold-skipped run as readers confined to different namespaces. The hold is
// another tenant's: the run is marketing's, the dataset was broken by a finance
// producer.
//
// A marketing reader still learns which dataset gated its run and the hold id
// (its own job declares the dataset, and its task rows carry both), but not the
// assertion, the values or the finance run that broke it. When the producer is
// marketing's own, the same reader gets the full line. An unconfined reader
// always does. Needs no policy file, so it also runs on the coverage auth lane.
func (s *IntegrationTestSuite) TestHoldWhyProvenanceIsConfinedToReadableNamespaces() {
	s.requireAuthLane()
	s.requireDataAssertionsLane()

	stamp := time.Now().UnixNano()
	foreignProducer := fmt.Sprintf("f5-foreign-producer-%d", stamp)
	foreignConsumer := fmt.Sprintf("f5-foreign-consumer-%d", stamp)
	ownProducer := fmt.Sprintf("f5-own-producer-%d", stamp)
	ownConsumer := fmt.Sprintf("f5-own-consumer-%d", stamp)
	foreignData := fmt.Sprintf("f5.foreign.%d", stamp)
	ownData := fmt.Sprintf("f5.own.%d", stamp)
	s.cleanupInOrder(foreignConsumer, foreignProducer, ownConsumer, ownProducer)
	marketing := s.mintNamespacedKey("viewer", "marketing")

	// A finance producer breaches; a marketing consumer is refused because of it.
	finance := s.holdProducerIn("finance", foreignProducer, foreignData, 12)
	breach := s.runProducer(finance)
	consumer := s.holdConsumerIn("marketing", foreignConsumer, foreignData)
	held := s.triggerHeldRun(consumer.ID)
	holdID := s.holdIDFromSkippedRun(held)
	path := fmt.Sprintf("/v1/jobs/%s/runs/%s/why?task=consume", consumer.ID, held.ID)
	whyArgs := []string{"why", held.ID, "--job-id", consumer.ID, "--task", "consume", "--server", s.caesiumURL}

	full := fmt.Sprintf("SKIPPED — dataset /%s held since run %s (assertion rowCount.min: observed 12 vs bound 1000; hold %s)",
		foreignData, breach.ID, holdID)
	redacted := fmt.Sprintf("SKIPPED — dataset /%s held (assertion detail withheld; hold %s)", foreignData, holdID)

	// The unconfined reader (the lane's admin key) gets the full line.
	s.Equal(full, firstLine(s.whyStdout(consumer.ID, held.ID, "consume")))

	// The marketing reader gets the redacted one, over REST and over the CLI.
	status, _, body := s.nsGet(path, marketing.Plaintext)
	s.Require().Equal(http.StatusOK, status, body)
	s.Contains(body, redacted)
	s.NotContains(body, breach.ID, "the finance run that broke the dataset is finance's")
	s.NotContains(body, "rowCount", "the assertion is finance's")
	s.NotContains(body, `"observed"`)
	s.NotContains(body, `"bound"`)
	s.NotContains(body, "finance")
	s.Contains(body, `"detail":"withheld"`)

	stdout, stderr, err := s.runCLIWithEnv([]string{"CAESIUM_API_KEY=" + marketing.Plaintext}, whyArgs...)
	s.Require().NoError(err, stderr)
	s.Equal(redacted, firstLine(stdout))
	s.NotContains(stdout, breach.ID)
	s.NotContains(stdout, "rowCount")
	s.NotContains(stdout, "finance")

	// A producer in the reader's own namespace is the reader's to see.
	marketingProducer := s.holdProducerIn("marketing", ownProducer, ownData, 12)
	ownBreach := s.runProducer(marketingProducer)
	ownRun := s.holdConsumerIn("marketing", ownConsumer, ownData)
	ownHeld := s.triggerHeldRun(ownRun.ID)
	ownHoldID := s.holdIDFromSkippedRun(ownHeld)
	status, _, body = s.nsGet(fmt.Sprintf("/v1/jobs/%s/runs/%s/why?task=consume", ownRun.ID, ownHeld.ID), marketing.Plaintext)
	s.Require().Equal(http.StatusOK, status, body)
	s.Contains(body, fmt.Sprintf("SKIPPED — dataset /%s held since run %s (assertion rowCount.min: observed 12 vs bound 1000; hold %s)",
		ownData, ownBreach.ID, ownHoldID))
}
