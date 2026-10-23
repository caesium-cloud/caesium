//go:build integration

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Identity-and-access A6: the deprecated shared secrets are audited and
// never required with a principal; POST /v1/events delivers only inside the
// caller's runner scope and reports the rest; manual fire is confined to the
// caller's scope; arrivals advance only datasets whose producers the scope
// covers; and, while a policy file is loaded, the webhook and lifecycle
// bridges stay inside the namespaces that spoke.

type a6IngestResponse struct {
	EventID           string   `json:"event_id"`
	MatchedTriggers   int      `json:"matched_triggers"`
	RunsStarted       int      `json:"runs_started"`
	Delivered         int      `json:"delivered"`
	Skipped           int      `json:"skipped"`
	SkippedNamespaces []string `json:"skipped_namespaces"`
}

func (s *IntegrationTestSuite) a6EngineLine() string {
	if s.engineType != "" && s.engineType != "docker" {
		return "\n    engine: " + s.engineType
	}
	return ""
}

// a6EventJobManifest is a job in namespace triggered by events of eventType
// (and, when filterLine is set, the extra YAML under the pattern).
func (s *IntegrationTestSuite) a6EventJobManifest(alias, namespace, eventType, patternExtra string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  namespace: %s
trigger:
  type: event
  configuration:
    events:
      - type: %s%s
steps:
  - name: record
    image: busybox:1.36.1%s
    command: ["sh", "-c", "echo a6"]
`, alias, namespace, eventType, patternExtra, s.a6EngineLine())
}

// a6ArrivalSourceManifest is a never-firing job in namespace declaring dataset
// as a source advanced by events of eventType.
func (s *IntegrationTestSuite) a6ArrivalSourceManifest(alias, namespace, dataset, eventType string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  namespace: %s
  datasets:
    sources:
      - name: %s
        expectedEvery: 24h
        arrival:
          event:
            type: %s
          watermark: "$.marker"
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: read
    image: busybox:1.36.1%s
    command: ["sh", "-c", "echo a6"]
    datasets:
      consumes:
        - %s
`, alias, namespace, dataset, eventType, s.a6EngineLine(), dataset)
}

// a6Apply applies a manifest with the lane's credentials, returns the job and
// deletes it when the test ends.
func (s *IntegrationTestSuite) a6Apply(alias, manifest string) *jobSummary {
	s.T().Helper()
	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	out, errOut, err := s.runCLIWithEnv([]string{"CAESIUM_FRESHNESS_ENABLED=true"}, "job", "apply", "--path", dir, "--server", s.caesiumURL)
	s.Require().NoError(err, "apply %s: %s %s", alias, out, errOut)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)
	s.T().Cleanup(func() {
		resp, err := s.doRequest(http.MethodDelete, s.caesiumURL+"/v1/jobs/"+job.ID, nil)
		if err == nil {
			_ = resp.Body.Close()
		}
	})
	return job
}

// a6Ingest posts an event as token (no shared secret) and decodes the 202.
func (s *IntegrationTestSuite) a6Ingest(token, payload string) (a6IngestResponse, string) {
	s.T().Helper()
	status, body := s.nsRequest(http.MethodPost, "/v1/events", token, payload, nil)
	s.Require().Equal(http.StatusAccepted, status, body)
	var resp a6IngestResponse
	s.Require().NoError(json.Unmarshal([]byte(body), &resp), body)
	s.Require().NotNil(resp.SkippedNamespaces, "skipped_namespaces is always a list: %s", body)
	return resp, body
}

func (s *IntegrationTestSuite) a6RunCount(jobID string) int {
	s.T().Helper()
	var runs []runResponse
	s.Require().NoError(s.tryGetJSON("/v1/jobs/"+jobID+"/runs", &runs))
	return len(runs)
}

func (s *IntegrationTestSuite) a6LegacyRows(actor string) []auditEntry {
	s.T().Helper()
	return runCLIJSON[[]auditEntry](s, "auth", "audit", "--actor", actor, "--action", "legacy_secret.used", "--limit", "200", "--server", s.caesiumURL)
}

func a6Metadata(s *IntegrationTestSuite, entry auditEntry) map[string]any {
	var metadata map[string]any
	s.Require().NoError(json.Unmarshal(entry.Metadata, &metadata))
	return metadata
}

// TestAuthLegacySecretAudited: a principal presenting the deprecated shared
// secret is audited under the secret's legacy actor, against the routed
// jobs' namespace, read back through `caesium auth audit` (stdout parsed);
// the same principal without the secret is accepted and leaves no row. No
// policy file is needed (it also runs on the coverage journey's lane).
func (s *IntegrationTestSuite) TestAuthLegacySecretAudited() {
	s.requireAuthLane()
	stamp := time.Now().UnixNano()
	eventType := fmt.Sprintf("a6.legacy.%d", stamp)
	eventAlias := fmt.Sprintf("a6-legacy-event-%d", stamp)
	s.a6Apply(eventAlias, s.a6EventJobManifest(eventAlias, "default", eventType, ""))

	withSecret := s.postEvent(fmt.Sprintf(`{"type":%q,"source":"a6","data":{}}`, eventType))
	s.Equal(1, withSecret.MatchedTriggers)
	var found *auditEntry
	rows := s.a6LegacyRows("legacy:event-ingest")
	for i := range rows {
		if a6Metadata(s, rows[i])["event_id"] == withSecret.EventID {
			found = &rows[i]
		}
	}
	s.Require().NotNil(found, "the ingest that presented the secret is audited: %+v", rows)
	s.Equal("success", found.Outcome)
	s.Equal("default", found.Namespace)
	metadata := a6Metadata(s, *found)
	s.Equal(float64(1), metadata["delivered"])
	s.NotEmpty(metadata["principal"], "the authenticated principal is recorded beside the legacy actor")
	s.NotContains(string(found.Metadata), s.eventIngestAPIKey)

	bearerOnly, _ := s.a6Ingest(s.authAPIKey, fmt.Sprintf(`{"type":%q,"source":"a6","data":{}}`, eventType))
	s.Equal(1, bearerOnly.Delivered)
	s.Equal(0, bearerOnly.Skipped)
	for _, row := range s.a6LegacyRows("legacy:event-ingest") {
		s.NotEqual(bearerOnly.EventID, a6Metadata(s, row)["event_id"], "no secret presented, no legacy row")
	}

	httpAlias := fmt.Sprintf("a6-legacy-http-%d", stamp)
	s.applyDefinition(s.namespacedHTTPJobDefinition(httpAlias, "default"))
	httpJob := s.requireJobByAlias(httpAlias)
	s.T().Cleanup(func() {
		if resp, err := s.doRequest(http.MethodDelete, s.caesiumURL+"/v1/jobs/"+httpJob.ID, nil); err == nil {
			_ = resp.Body.Close()
		}
	})
	triggerID := stringFromMap(s.jobDetailByAlias(httpAlias), "trigger_id")
	s.Require().NotEmpty(triggerID)
	resp, err := s.doManualTriggerRequest(http.MethodPost, s.caesiumURL+"/v1/triggers/"+triggerID+"/fire", bytes.NewBufferString(`{}`))
	s.Require().NoError(err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	s.Require().Equal(http.StatusAccepted, resp.StatusCode, string(body))
	found = nil
	rows = s.a6LegacyRows("legacy:manual-trigger")
	for i := range rows {
		if a6Metadata(s, rows[i])["trigger_id"] == triggerID {
			found = &rows[i]
		}
	}
	s.Require().NotNil(found, "the fire that presented the secret is audited: %+v", rows)
	s.Equal("success", found.Outcome)
	s.Equal("default", found.Namespace)
}

// TestAuthEventIngestNamespaced: a marketing runner key's event reaches only
// the marketing subscriber; the response reports the finance one as skipped
// without naming it; the cluster admin reaches both. A job-scoped key is
// confined to its exact aliases even though its role is cluster-wide.
func (s *IntegrationTestSuite) TestAuthEventIngestNamespaced() {
	s.requireAuthLane()
	s.requirePolicyFixture()
	stamp := time.Now().UnixNano()
	eventType := fmt.Sprintf("a6.ingest.%d", stamp)
	mktAlias, finAlias := fmt.Sprintf("a6-mkt-sub-%d", stamp), fmt.Sprintf("a6-fin-sub-%d", stamp)
	mkt := s.a6Apply(mktAlias, s.a6EventJobManifest(mktAlias, "marketing", eventType, ""))
	fin := s.a6Apply(finAlias, s.a6EventJobManifest(finAlias, "finance", eventType, ""))
	payload := fmt.Sprintf(`{"type":%q,"source":"a6","data":{}}`, eventType)

	mktRunner := s.mintNamespacedKey("runner", "marketing").Plaintext
	resp, body := s.a6Ingest(mktRunner, payload)
	s.Equal(1, resp.MatchedTriggers)
	s.Equal(1, resp.Delivered)
	s.Equal(1, resp.Skipped)
	s.Equal([]string{}, resp.SkippedNamespaces, "the marketing runner may not read finance")
	s.NotContains(body, "finance")
	s.NotContains(body, finAlias)
	s.Require().Eventually(func() bool { return s.a6RunCount(mkt.ID) == 1 }, 2*time.Minute, time.Second,
		"the marketing subscriber runs (quota one: it may queue first)")
	s.Equal(0, s.a6RunCount(fin.ID), "the finance subscriber never received the marketing key's event")

	jobKey := s.createAPIKeyCLI("--role", "runner", "--jobs", finAlias, "--description", fmt.Sprintf("a6 job-scoped %d", stamp))
	s.T().Cleanup(func() {
		_, _ = s.runCLIStdout("auth", "key", "revoke", "--id", jobKey.Meta.ID, "--server", s.caesiumURL)
	})
	resp, _ = s.a6Ingest(jobKey.Plaintext, payload)
	s.Equal(1, resp.Delivered)
	s.Equal(1, resp.Skipped)
	s.Equal([]string{}, resp.SkippedNamespaces, "a job-scoped key reads no other job")
	s.Require().Eventually(func() bool { return s.a6RunCount(fin.ID) == 1 }, 2*time.Minute, time.Second)

	resp, _ = s.a6Ingest(s.authAPIKey, payload)
	s.Equal(2, resp.MatchedTriggers)
	s.Equal(2, resp.Delivered)
	s.Equal(0, resp.Skipped)
	s.Equal([]string{}, resp.SkippedNamespaces)
}

// TestAuthTriggerFireConfined: a marketing runner fires its own HTTP trigger
// without any shared secret (the principal decides), twice in a row against
// marketing's quota of one (queued, never refused); a job-scoped key fires a
// trigger whose jobs are all its own and is refused on a foreign one.
func (s *IntegrationTestSuite) TestAuthTriggerFireConfined() {
	s.requireAuthLane()
	s.requirePolicyFixture()
	stamp := time.Now().UnixNano()
	mktAlias, finAlias := fmt.Sprintf("a6-fire-mkt-%d", stamp), fmt.Sprintf("a6-fire-fin-%d", stamp)
	for _, def := range []struct{ alias, namespace string }{{mktAlias, "marketing"}, {finAlias, "finance"}} {
		s.applyDefinition(s.namespacedHTTPJobDefinition(def.alias, def.namespace))
		job := s.requireJobByAlias(def.alias)
		s.T().Cleanup(func() {
			if resp, err := s.doRequest(http.MethodDelete, s.caesiumURL+"/v1/jobs/"+job.ID, nil); err == nil {
				_ = resp.Body.Close()
			}
		})
	}
	mktJob := s.requireJobByAlias(mktAlias)
	mktTrigger := stringFromMap(s.jobDetailByAlias(mktAlias), "trigger_id")
	finTrigger := stringFromMap(s.jobDetailByAlias(finAlias), "trigger_id")

	mktRunner := s.mintNamespacedKey("runner", "marketing").Plaintext
	for range 2 {
		s.requireNSStatus(http.StatusAccepted, http.MethodPost, "/v1/triggers/"+mktTrigger+"/fire", mktRunner, "")
	}
	s.Require().Eventually(func() bool { return s.a6RunCount(mktJob.ID) == 2 }, 2*time.Minute, time.Second,
		"both fires run; the second waits for marketing's quota slot")
	s.requireNSStatus(http.StatusForbidden, http.MethodPost, "/v1/triggers/"+finTrigger+"/fire", mktRunner, "")

	jobKey := s.createAPIKeyCLI("--role", "runner", "--jobs", mktAlias, "--description", fmt.Sprintf("a6 fire %d", stamp))
	s.T().Cleanup(func() {
		_, _ = s.runCLIStdout("auth", "key", "revoke", "--id", jobKey.Meta.ID, "--server", s.caesiumURL)
	})
	s.requireNSStatus(http.StatusAccepted, http.MethodPost, "/v1/triggers/"+mktTrigger+"/fire", jobKey.Plaintext, "")
	s.Require().Eventually(func() bool { return s.a6RunCount(mktJob.ID) == 3 }, 2*time.Minute, time.Second)
	s.requireNSStatus(http.StatusForbidden, http.MethodPost, "/v1/triggers/"+finTrigger+"/fire", jobKey.Plaintext, "")
}

// TestAuthArrivalStaysInNamespace: an arrival advances a source dataset only
// when the caller's scope covers its producers. A marketing runner's event
// advances marketing's dataset but neither finance's nor a dataset nobody
// produces (cluster-only); the cluster admin's advances both.
func (s *IntegrationTestSuite) TestAuthArrivalStaysInNamespace() {
	s.requireAuthLane()
	s.requirePolicyFixture()
	s.requireFreshnessLane()
	stamp := time.Now().UnixNano()
	eventType := fmt.Sprintf("a6.arrival.%d", stamp)
	mktData, finData, extData := fmt.Sprintf("a6.mkt.%d", stamp), fmt.Sprintf("a6.fin.%d", stamp), fmt.Sprintf("a6.ext.%d", stamp)
	for _, job := range []struct{ alias, manifest string }{
		{fmt.Sprintf("a6-mkt-prod-%d", stamp), namespacedProducerManifest(fmt.Sprintf("a6-mkt-prod-%d", stamp), "marketing", mktData)},
		{fmt.Sprintf("a6-fin-prod-%d", stamp), namespacedProducerManifest(fmt.Sprintf("a6-fin-prod-%d", stamp), "finance", finData)},
		{fmt.Sprintf("a6-mkt-src-%d", stamp), s.a6ArrivalSourceManifest(fmt.Sprintf("a6-mkt-src-%d", stamp), "marketing", mktData, eventType)},
		{fmt.Sprintf("a6-fin-src-%d", stamp), s.a6ArrivalSourceManifest(fmt.Sprintf("a6-fin-src-%d", stamp), "finance", finData, eventType)},
		{fmt.Sprintf("a6-ext-src-%d", stamp), s.a6ArrivalSourceManifest(fmt.Sprintf("a6-ext-src-%d", stamp), "marketing", extData, eventType)},
	} {
		s.a6Apply(job.alias, job.manifest)
	}
	watermark := func(name string) string {
		var detail datasetDetailResponse
		s.getJSON("/v1/datasets/_/"+name, &detail)
		return detail.State.Watermark
	}

	mktRunner := s.mintNamespacedKey("runner", "marketing").Plaintext
	s.a6Ingest(mktRunner, fmt.Sprintf(`{"type":%q,"source":"a6","data":{"marker":"m-%d"}}`, eventType, stamp))
	s.Require().Eventually(func() bool { return watermark(mktData) == fmt.Sprintf("m-%d", stamp) }, 30*time.Second, 500*time.Millisecond,
		"the marketing runner advances its own namespace's dataset")
	s.Empty(watermark(finData), "a marketing credential never advances a finance watermark")
	s.Empty(watermark(extData), "a dataset nobody produces is cluster-only for arrivals")

	s.a6Ingest(s.authAPIKey, fmt.Sprintf(`{"type":%q,"source":"a6","data":{"marker":"a-%d"}}`, eventType, stamp))
	s.Require().Eventually(func() bool {
		return watermark(finData) == fmt.Sprintf("a-%d", stamp) && watermark(extData) == fmt.Sprintf("a-%d", stamp)
	}, 30*time.Second, 500*time.Millisecond, "the cluster admin advances every matching dataset, as before")
}

// TestWebhookBridgeStaysInNamespace (no-auth lane, policy file loaded): a
// webhook accepted by a marketing HTTP trigger is bridged only to marketing's
// event subscribers and advances only marketing-produced datasets.
func (s *IntegrationTestSuite) TestWebhookBridgeStaysInNamespace() {
	s.requirePolicyFixture()
	stamp := time.Now().UnixNano()
	source := fmt.Sprintf("a6-hook-%d", stamp)
	httpAlias := fmt.Sprintf("a6-hook-http-%d", stamp)
	s.applyDefinition(s.namespacedHTTPJobDefinition(httpAlias, "marketing"))
	httpJob := s.requireJobByAlias(httpAlias)
	s.T().Cleanup(func() {
		if resp, err := s.doRequest(http.MethodDelete, s.caesiumURL+"/v1/jobs/"+httpJob.ID, nil); err == nil {
			_ = resp.Body.Close()
		}
	})
	pattern := "\n        source: " + source
	mktAlias, finAlias := fmt.Sprintf("a6-hook-mkt-%d", stamp), fmt.Sprintf("a6-hook-fin-%d", stamp)
	mkt := s.a6Apply(mktAlias, s.a6EventJobManifest(mktAlias, "marketing", "webhook", pattern))
	fin := s.a6Apply(finAlias, s.a6EventJobManifest(finAlias, "finance", "webhook", pattern))

	freshness := false
	var features freshnessFeatures
	if err := s.tryGetJSON("/v1/system/features", &features); err == nil {
		freshness = features.FreshnessEnabled
	}
	mktData, finData := fmt.Sprintf("a6.hook.mkt.%d", stamp), fmt.Sprintf("a6.hook.fin.%d", stamp)
	if freshness {
		for _, job := range []struct{ alias, manifest string }{
			{fmt.Sprintf("a6-hook-mkt-prod-%d", stamp), namespacedProducerManifest(fmt.Sprintf("a6-hook-mkt-prod-%d", stamp), "marketing", mktData)},
			{fmt.Sprintf("a6-hook-fin-prod-%d", stamp), namespacedProducerManifest(fmt.Sprintf("a6-hook-fin-prod-%d", stamp), "finance", finData)},
			{fmt.Sprintf("a6-hook-mkt-src-%d", stamp), s.a6ArrivalSourceManifest(fmt.Sprintf("a6-hook-mkt-src-%d", stamp), "marketing", mktData, "webhook")},
			{fmt.Sprintf("a6-hook-fin-src-%d", stamp), s.a6ArrivalSourceManifest(fmt.Sprintf("a6-hook-fin-src-%d", stamp), "finance", finData, "webhook")},
		} {
			s.a6Apply(job.alias, job.manifest)
		}
	}

	marker := fmt.Sprintf("hook-%d", stamp)
	req, err := http.NewRequestWithContext(s.T().Context(), http.MethodPost, s.caesiumURL+"/v1/hooks/hooks/"+httpAlias, strings.NewReader(fmt.Sprintf(`{"marker":%q}`, marker)))
	s.Require().NoError(err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Caesium-Event-Source", source)
	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	s.Require().Equal(http.StatusAccepted, resp.StatusCode, string(body))

	s.Require().Eventually(func() bool { return s.a6RunCount(mkt.ID) == 1 }, 2*time.Minute, time.Second,
		"the marketing subscriber receives the marketing webhook")
	s.Equal(0, s.a6RunCount(fin.ID), "the finance subscriber is outside the webhook's namespaces")
	if freshness {
		s.Require().Eventually(func() bool {
			var detail datasetDetailResponse
			s.getJSON("/v1/datasets/_/"+mktData, &detail)
			return detail.State.Watermark == marker
		}, 30*time.Second, 500*time.Millisecond)
		var detail datasetDetailResponse
		s.getJSON("/v1/datasets/_/"+finData, &detail)
		s.Empty(detail.State.Watermark, "a marketing webhook never advances a finance watermark")
	}
}

// TestLifecycleBridgeStaysInNamespace (no-auth lane, policy file loaded): a
// marketing run's completion reaches a marketing subscriber but not a finance
// one listening for the same job.
func (s *IntegrationTestSuite) TestLifecycleBridgeStaysInNamespace() {
	s.requirePolicyFixture()
	replay := s.a6LifecycleReplayControl()
	if replay != nil {
		// Release the publisher before job-deletion cleanups can wait on it.
		defer replay.disarm(s)
	}
	stamp := time.Now().UnixNano()
	sourceAlias := fmt.Sprintf("a6-life-src-%d", stamp)
	s.applyDefinition(s.namespacedJobDefinition(sourceAlias, "marketing"))
	sourceJob := s.requireJobByAlias(sourceAlias)
	s.T().Cleanup(func() {
		if resp, err := s.doRequest(http.MethodDelete, s.caesiumURL+"/v1/jobs/"+sourceJob.ID, nil); err == nil {
			_ = resp.Body.Close()
		}
	})
	pattern := "\n        source: caesium\n        filter:\n          job_alias: " + sourceAlias
	mktAlias, finAlias := fmt.Sprintf("a6-life-mkt-%d", stamp), fmt.Sprintf("a6-life-fin-%d", stamp)
	mkt := s.a6Apply(mktAlias, s.a6EventJobManifest(mktAlias, "marketing", "run_completed", pattern))
	fin := s.a6Apply(finAlias, s.a6EventJobManifest(finAlias, "finance", "run_completed", pattern))
	var barrier *jobSummary
	if replay != nil {
		// This job cannot match either subscriber's source-alias filter. Its
		// completion later proves that the bridge consumed the earlier replay.
		barrierAlias := fmt.Sprintf("a6-life-barrier-%d", stamp)
		s.applyDefinition(s.namespacedJobDefinition(barrierAlias, "default"))
		barrier = s.requireJobByAlias(barrierAlias)
		s.T().Cleanup(func() {
			if resp, err := s.doRequest(http.MethodDelete, s.caesiumURL+"/v1/jobs/"+barrier.ID, nil); err == nil {
				_ = resp.Body.Close()
			}
		})
		s.T().Logf("lifecycle replay owned jobs source=%s/%s marketing=%s/%s finance=%s/%s barrier=%s/%s",
			sourceJob.ID, sourceJob.Alias, mkt.ID, mkt.Alias, fin.ID, fin.Alias, barrier.ID, barrier.Alias)
		replay.arm(s, "", "run_completed")
	}

	resp, err := s.doJSONRequest(http.MethodPost, s.caesiumURL+"/v1/jobs/"+sourceJob.ID+"/run", strings.NewReader(`{}`))
	s.Require().NoError(err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	s.Require().Equal(http.StatusAccepted, resp.StatusCode, string(body))
	var sourceRun runResponse
	if replay != nil {
		s.Require().NoError(json.Unmarshal(body, &sourceRun))
		s.Require().NotEmpty(sourceRun.ID)
		replay.arm(s, sourceRun.ID, "run_completed")
		replay.exercise(s, sourceJob, sourceRun.ID, barrier)
		replay.closeout(s, sourceJob, sourceRun.ID, mkt, fin)
	}

	s.Require().Eventually(func() bool { return s.a6RunCount(mkt.ID) == 1 }, 3*time.Minute, time.Second,
		"the marketing subscriber receives the marketing run's completion")
	s.Equal(0, s.a6RunCount(fin.ID), "the finance subscriber never sees a marketing run's lifecycle")
}

// The overlay uses the existing file-based testfault control in a fresh
// single-server instrumented lane. The runner and server mount the SAME owned
// directory, and receive the same CAESIUM_RUN_QUEUE_DEQUEUE_INTERVAL. An
// ordinary integration lane still exercises the case above without a skip.
const a6LifecycleFaultDirEnv = "CAESIUM_LIFECYCLE_TESTFAULT_DIR"

type a6LifecycleReplay struct {
	dir      string
	interval time.Duration
	sequence uint64
	eventID  string
}

type a6LifecycleHook struct {
	Banner   string `json:"banner"`
	Phase    string `json:"phase"`
	Path     string `json:"path"`
	Type     string `json:"type"`
	RunID    string `json:"run_id"`
	Sequence uint64 `json:"sequence"`
	Reason   string `json:"reason"`
}

type a6LifecycleEvent struct {
	ID   string `json:"id"`
	Data struct {
		RunID    string `json:"run_id"`
		JobID    string `json:"job_id"`
		JobAlias string `json:"job_alias"`
		Sequence uint64 `json:"sequence"`
	} `json:"data"`
}

type a6LifecycleMatch struct {
	MatchID     string   `json:"match_id"`
	EventID     string   `json:"event_id"`
	RunsStarted []string `json:"runs_started"`
	Skipped     bool     `json:"skipped"`
	SkipReason  string   `json:"skip_reason"`
	Error       string   `json:"error"`
}

func (s *IntegrationTestSuite) a6LifecycleReplayControl() *a6LifecycleReplay {
	s.T().Helper()
	dir := os.Getenv(a6LifecycleFaultDirEnv)
	if dir == "" {
		s.T().Log("lifecycle replay mode=ordinary (no fault overlay)")
		return nil
	}
	s.requireMarketingQuota()
	s.Require().True(filepath.IsAbs(dir), "%s must be an absolute shared directory", a6LifecycleFaultDirEnv)
	info, err := os.Stat(dir)
	s.Require().NoError(err)
	s.Require().True(info.IsDir(), "fault control path must be a directory")
	interval, err := time.ParseDuration(os.Getenv("CAESIUM_RUN_QUEUE_DEQUEUE_INTERVAL"))
	s.Require().NoError(err, "instrumented runner must receive the server's explicit dequeue interval")
	s.Require().Greater(interval, time.Duration(0))
	s.Require().LessOrEqual(interval, time.Second, "bound the ten-interval closeout to ten seconds")
	path := filepath.Join(dir, "bus-publish-pause.json")
	// Never take over an already armed directory or remove a foreign control.
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	s.Require().NoError(err, "fault directory must have no existing directive")
	s.T().Cleanup(func() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			s.T().Errorf("release owned lifecycle pause: %v", err)
		}
	})
	s.Require().NoError(fh.Close())
	s.T().Logf("lifecycle replay mode=instrumented dir=%s dequeue_interval=%s", dir, interval)
	return &a6LifecycleReplay{dir: dir, interval: interval}
}

func (r *a6LifecycleReplay) arm(s *IntegrationTestSuite, runID, eventType string) {
	s.T().Helper()
	raw, err := json.Marshal(map[string]any{
		"armed": true, "run_id": runID, "types": []string{eventType},
		"paths": []string{"publish_and_mark"}, "max_hold_ms": 180000,
	})
	s.Require().NoError(err)
	fh, err := os.CreateTemp(r.dir, ".a6-lifecycle-pause-*")
	s.Require().NoError(err)
	defer fh.Close()
	defer os.Remove(fh.Name())
	_, err = fh.Write(raw)
	s.Require().NoError(err)
	s.Require().NoError(fh.Close())
	// Retarget atomically: an absent/partial directive would release both
	// events and lose the proof that completion was published before terminal.
	s.Require().NoError(os.Rename(fh.Name(), filepath.Join(r.dir, "bus-publish-pause.json")))
}

func (r *a6LifecycleReplay) disarm(s *IntegrationTestSuite) {
	s.T().Helper()
	if err := os.Remove(filepath.Join(r.dir, "bus-publish-pause.json")); err != nil && !os.IsNotExist(err) {
		s.T().Errorf("release owned lifecycle pause: %v", err)
	}
}

func (r *a6LifecycleReplay) hooks(s *IntegrationTestSuite, runID, eventType, phase string) []a6LifecycleHook {
	s.T().Helper()
	raw, err := os.ReadFile(filepath.Join(r.dir, "bus-publish-hook.log"))
	if os.IsNotExist(err) {
		return nil
	}
	s.Require().NoError(err)
	var found []a6LifecycleHook
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var entry a6LifecycleHook
		s.Require().NoError(json.Unmarshal(line, &entry), "invalid hook evidence: %s", line)
		s.Require().Equal("caesium-testfault-control", entry.Banner)
		if entry.RunID == runID && entry.Type == eventType && entry.Phase == phase {
			s.Require().Equal("publish_and_mark", entry.Path)
			s.Require().NotZero(entry.Sequence)
			found = append(found, entry)
		}
	}
	return found
}

func (s *IntegrationTestSuite) a6LifecycleIngestions(runID string) []a6LifecycleEvent {
	s.T().Helper()
	var found []a6LifecycleEvent
	for offset := 0; ; offset += 500 {
		s.Require().Less(offset, 10000, "bounded lifecycle history read must finish without truncation")
		var page []a6LifecycleEvent
		s.getJSON(fmt.Sprintf("/v1/events/ingested?type=run_completed&source=caesium&limit=500&offset=%d", offset), &page)
		for _, evt := range page {
			if evt.Data.RunID == runID {
				found = append(found, evt)
			}
		}
		if len(page) < 500 {
			return found
		}
	}
}

// Poll on the test goroutine: hook/history validation deliberately fails
// closed, and require.FailNow must not run in Eventually's worker goroutine.
func (s *IntegrationTestSuite) a6AwaitLifecycle(bound, interval time.Duration, condition func() bool, message string) {
	s.T().Helper()
	deadline := time.Now().Add(bound)
	for {
		if condition() {
			return
		}
		if !time.Now().Before(deadline) {
			s.Require().FailNow(message)
		}
		time.Sleep(interval)
	}
}

func (s *IntegrationTestSuite) a6LifecycleDrops() float64 {
	s.T().Helper()
	resp, err := s.doRequest(http.MethodGet, s.caesiumURL+"/metrics", nil)
	s.Require().NoError(err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, resp.StatusCode, string(body))
	// CounterVec emits no series before its first drop. The registered bus
	// counter's absent run_completed child therefore starts at zero.
	value, _, err := parsePromLabeledCounter(string(body), "caesium_event_bus_dropped_total", map[string]string{"event_type": "run_completed"})
	s.Require().NoError(err)
	return value
}

func (r *a6LifecycleReplay) exercise(s *IntegrationTestSuite, source *jobSummary, runID string, barrier *jobSummary) {
	s.T().Helper()
	s.a6AwaitLifecycle(runTimeout, 50*time.Millisecond, func() bool {
		entries := r.hooks(s, runID, "run_completed", "enter")
		if len(entries) == 0 {
			return false
		}
		s.Require().Len(entries, 1)
		r.sequence = entries[0].Sequence
		return true
	}, "source completion never entered the instrumented immediate-publication hook")
	s.Require().Equal("succeeded", s.awaitRun(source.ID, runID, runTimeout).Status)
	var initial []a6LifecycleEvent
	s.a6AwaitLifecycle(30*time.Second, 100*time.Millisecond, func() bool {
		initial = s.a6LifecycleIngestions(runID)
		return len(initial) > 0
	}, "background dispatcher never ingested the held durable completion")
	s.Require().Empty(r.hooks(s, runID, "run_completed", "release"), "initial ingestion must precede immediate publication")
	s.Require().Len(initial, 1)
	s.Require().Equal(r.sequence, initial[0].Data.Sequence)
	s.Require().Equal(source.ID, initial[0].Data.JobID)
	s.Require().Equal(source.Alias, initial[0].Data.JobAlias)
	s.Require().NotEmpty(initial[0].ID)
	r.eventID = initial[0].ID
	s.T().Logf("lifecycle replay background delivery run=%s sequence=%d event_id=%s while immediate publication held", runID, r.sequence, r.eventID)
	dropsBefore := s.a6LifecycleDrops()

	// PublishAndMarkBusDispatched publishes completion, then terminal, in the
	// same goroutine. Holding terminal proves completion's bus.Publish returned.
	r.arm(s, runID, "run_terminal")
	var terminalSequence uint64
	s.a6AwaitLifecycle(10*time.Second, 50*time.Millisecond, func() bool {
		entries := r.hooks(s, runID, "run_terminal", "enter")
		if len(entries) == 0 {
			return false
		}
		s.Require().Len(entries, 1)
		terminalSequence = entries[0].Sequence
		return true
	}, "immediate path did not publish the completion before reaching terminal")
	completedRelease := r.hooks(s, runID, "run_completed", "release")
	s.Require().Len(completedRelease, 1)
	s.Require().Equal(r.sequence, completedRelease[0].Sequence)
	s.Require().Equal("disarmed", completedRelease[0].Reason)
	s.Require().Greater(terminalSequence, r.sequence)

	// Bus.Publish synchronously enqueues into the lifecycle subscription, and
	// RunLifecycleBridge consumes that one channel serially. This later HTTP
	// run's ingested completion is a watermark AFTER consumption of the replay;
	// SSE cannot provide this proof because it hides repeated sequences.
	barrierRunID := s.triggerRun(barrier.ID)
	s.Require().Equal("succeeded", s.awaitRun(barrier.ID, barrierRunID, runTimeout).Status)
	s.a6AwaitLifecycle(30*time.Second, 100*time.Millisecond, func() bool {
		return len(s.a6LifecycleIngestions(barrierRunID)) > 0
	}, "lifecycle bridge never consumed the post-replay barrier")
	s.Require().Equal(dropsBefore, s.a6LifecycleDrops(), "a dropped completion would invalidate the FIFO replay proof")
	s.Require().Empty(r.hooks(s, runID, "run_terminal", "release"), "terminal pause expired before the FIFO barrier")
	s.Require().NoError(os.Remove(filepath.Join(r.dir, "bus-publish-pause.json")))
	s.a6AwaitLifecycle(10*time.Second, 50*time.Millisecond, func() bool {
		entries := r.hooks(s, runID, "run_terminal", "release")
		if len(entries) == 0 {
			return false
		}
		s.Require().Len(entries, 1)
		s.Require().Equal(terminalSequence, entries[0].Sequence)
		s.Require().Equal("disarmed", entries[0].Reason)
		return true
	}, "terminal hook never released under test control")
	s.T().Logf("lifecycle replay immediate delivery consumed run=%s sequence=%d terminal_sequence=%d barrier_run=%s", runID, r.sequence, terminalSequence, barrierRunID)
}

func (r *a6LifecycleReplay) closeout(s *IntegrationTestSuite, source *jobSummary, runID string, mkt, fin *jobSummary) {
	s.T().Helper()
	mktTrigger := stringFromMap(s.jobDetailByAlias(mkt.Alias), "trigger_id")
	finTrigger := stringFromMap(s.jobDetailByAlias(fin.Alias), "trigger_id")
	s.Require().NotEmpty(mktTrigger)
	s.Require().NotEmpty(finTrigger)
	assertExact := func() {
		ingested := s.a6LifecycleIngestions(runID)
		var matches, financeMatches []a6LifecycleMatch
		s.getJSON("/v1/triggers/"+mktTrigger+"/events?type=run_completed&source=caesium", &matches)
		s.getJSON("/v1/triggers/"+finTrigger+"/events?type=run_completed&source=caesium", &financeMatches)
		runs := s.fetchRuns(mkt.ID)
		queue, financeQueue := s.jobQueueREST(mkt.ID), s.jobQueueREST(fin.ID)
		s.T().Logf("lifecycle replay exact counts sequence=%d ingested=%d matches=%d marketing_runs=%d marketing_queue=%d finance_matches=%d finance_runs=%d finance_queue=%d",
			r.sequence, len(ingested), len(matches), len(runs), len(queue), len(financeMatches), s.a6RunCount(fin.ID), len(financeQueue))
		s.Require().Len(ingested, 1, "replayed durable completion must have one ingested row")
		s.Require().Equal(r.eventID, ingested[0].ID)
		s.Require().Equal(r.sequence, ingested[0].Data.Sequence)
		s.Require().Equal(source.ID, ingested[0].Data.JobID)
		s.Require().Equal(source.Alias, ingested[0].Data.JobAlias)
		s.Require().Len(matches, 1, "replay must not record another trigger match")
		s.Require().NotEmpty(matches[0].MatchID)
		s.Require().Equal(r.eventID, matches[0].EventID)
		s.Require().False(matches[0].Skipped)
		s.Require().Empty(matches[0].SkipReason)
		s.Require().Empty(matches[0].Error)
		s.Require().Len(runs, 1, "replay must not admit another marketing run")
		s.T().Logf("lifecycle replay sole marketing run=%s match=%s", runs[0].ID, matches[0].MatchID)
		s.Require().Equal([]string{runs[0].ID}, matches[0].RunsStarted)
		s.Require().Empty(queue, "replay must not leave a quota-held duplicate")
		s.Require().Empty(financeMatches)
		s.Require().Zero(s.a6RunCount(fin.ID))
		s.Require().Empty(financeQueue)
	}
	assertExact()
	runs := s.fetchRuns(mkt.ID)
	s.Require().Equal("succeeded", s.awaitRun(mkt.ID, runs[0].ID, runTimeout).Status)
	// The source and subscriber are terminal, so marketing's quota slot is
	// available. Observe elapsed time covering ten configured drain intervals:
	// a parked duplicate must not escape the earlier empty-queue/run assertions
	// by starting afterwards.
	deadline := time.Now().Add(10 * r.interval)
	for {
		assertExact()
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(r.interval / 2)
	}
	s.T().Logf("lifecycle replay closeout passed sequence=%d event_id=%s drain_intervals=10 interval=%s", r.sequence, r.eventID, r.interval)
}
