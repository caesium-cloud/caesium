package notification

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// Namespace-aware routing (identity-and-access D4): a policy delivers only
// events its own namespace owns, a "*" policy delivers everything, and a
// non-"*" destination receives a payload scoped to its namespace.

type routeFixture struct {
	t  *testing.T
	db *gorm.DB
}

// newRouteFixture serves a policy file: namespace isolation applies only
// while one is loaded (design §§1/15). newLegacyRouteFixture serves none.
func newRouteFixture(t *testing.T) *routeFixture {
	f := newLegacyRouteFixture(t)
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte("apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  marketing: {}\n  finance: {}\n"), 0o600))
	holder, err := policy.LoadFile(t.Context(), policy.HolderOptions{Path: path})
	require.NoError(t, err)
	policy.SetProcessHolder(holder)
	return f
}

func newLegacyRouteFixture(t *testing.T) *routeFixture {
	policy.SetProcessHolder(nil)
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	return &routeFixture{t: t, db: db}
}

func (f *routeFixture) job(alias, namespace string, labels map[string]any) uuid.UUID {
	f.t.Helper()
	now := time.Now().UTC()
	triggerID := uuid.New()
	require.NoError(f.t, f.db.Create(&models.Trigger{ID: triggerID, Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now}).Error)
	id := uuid.New()
	require.NoError(f.t, f.db.Create(&models.Job{ID: id, Alias: alias, Namespace: namespace, TriggerID: triggerID, Labels: datatypes.JSONMap(labels), CreatedAt: now, UpdatedAt: now}).Error)
	return id
}

func (f *routeFixture) run(jobID uuid.UUID, namespace string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	require.NoError(f.t, f.db.Create(&models.JobRun{ID: id, JobID: jobID, Namespace: namespace, Status: "succeeded", StartedAt: time.Now().UTC()}).Error)
	return id
}

func (f *routeFixture) incident(jobID uuid.UUID, namespace string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	require.NoError(f.t, f.db.Create(&models.Incident{ID: id, JobID: jobID, Namespace: namespace, Class: "unknown", Status: models.IncidentStatus("open"), DedupeKey: id.String()}).Error)
	return id
}

func (f *routeFixture) channel(name, namespace string, enabled bool) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	require.NoError(f.t, f.db.Create(&models.NotificationChannel{ID: id, Namespace: namespace, Name: name, Type: models.ChannelTypeWebhook, Config: datatypes.JSON(`{}`), Enabled: true}).Error)
	if !enabled {
		require.NoError(f.t, f.db.Model(&models.NotificationChannel{}).Where("id = ?", id).Update("enabled", false).Error)
	}
	return id
}

func (f *routeFixture) policy(name, namespace string, channel uuid.UUID, eventType event.Type, filters string) {
	f.t.Helper()
	p := models.NotificationPolicy{ID: uuid.New(), Namespace: namespace, Name: name, ChannelID: channel,
		EventTypes: datatypes.JSON(`["` + string(eventType) + `"]`), Enabled: true}
	if filters != "" {
		p.Filters = datatypes.JSON(filters)
	}
	require.NoError(f.t, f.db.Create(&p).Error)
}

// policies creates a channel and a policy for eventType in each namespace,
// named after the namespace.
func (f *routeFixture) policies(eventType event.Type, namespaces ...string) {
	f.t.Helper()
	for _, namespace := range namespaces {
		f.policy("pol-"+namespace, namespace, f.channel("ch-"+namespace, namespace, true), eventType, "")
	}
}

func (f *routeFixture) route(evt event.Event) map[string]Delivery {
	f.t.Helper()
	deliveries, err := RouteEvent(context.Background(), f.db, evt)
	require.NoError(f.t, err)
	byPolicy := map[string]Delivery{}
	for _, d := range deliveries {
		byPolicy[d.Policy.Name] = d
	}
	return byPolicy
}

func policyNames(deliveries map[string]Delivery) []string {
	names := make([]string, 0, len(deliveries))
	for name := range deliveries {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func runPayload(t *testing.T, alias string, labels map[string]string) json.RawMessage {
	t.Helper()
	return mustJSON(t, map[string]any{"job_alias": alias, "job_labels": labels, "status": "succeeded"})
}

func TestRouteEventMatchesTheOwningNamespaceAndShared(t *testing.T) {
	f := newRouteFixture(t)
	f.policies(event.TypeRunCompleted, "*", "marketing", "finance", "default")
	mkt := f.job("mkt-job", "marketing", nil)
	run := f.run(mkt, "marketing")

	got := f.route(event.Event{Type: event.TypeRunCompleted, JobID: mkt, RunID: run, Payload: runPayload(t, "mkt-job", nil)})
	require.Equal(t, []string{"pol-*", "pol-marketing"}, policyNames(got))

	// With a policy file a default policy never receives another namespace's
	// events.
	fin := f.job("fin-job", "finance", nil)
	got = f.route(event.Event{Type: event.TypeRunCompleted, JobID: fin, RunID: f.run(fin, "finance")})
	require.Equal(t, []string{"pol-*", "pol-finance"}, policyNames(got))
}

// TestRouteEventWithoutAPolicyFileMatchesAsBefore: with no policy file
// (design §§1/15) every policy matches by event type and filters, whatever
// its namespace and the event's, and receives the event exactly as built:
// no ownership, label, contract or baseline scoping. Channel eligibility
// (existing, enabled, visible from the policy) still applies.
func TestRouteEventWithoutAPolicyFileMatchesAsBefore(t *testing.T) {
	f := newLegacyRouteFixture(t)
	f.policies(event.TypeRunCompleted, "*", "marketing", "finance", "default")
	f.policy("pol-fin-labels", "finance", f.channel("ch-fin-labels", "finance", true), event.TypeRunCompleted, `{"labels":{"owner":"finance-team"}}`)
	f.policy("pol-foreign-channel", "default", f.channel("ch-foreign", "finance", true), event.TypeRunCompleted, "")
	job := f.job("moved-job", "finance", map[string]any{"owner": "finance-team"})
	run := f.run(job, "marketing")
	legacy := map[string]any{"assertion": "deltaFromBaseline", "observed": 5, "baseline_median": 10, "message": "below the baseline median 10"}
	evt := event.Event{Type: event.TypeRunCompleted, JobID: job, RunID: run, Payload: mustJSON(t, map[string]any{
		"job_alias": "moved-job", "job_labels": map[string]string{"owner": "finance-team"}, "data_violations": []map[string]any{legacy}})}

	got := f.route(evt)
	require.Equal(t, []string{"pol-*", "pol-default", "pol-fin-labels", "pol-finance", "pol-marketing"}, policyNames(got),
		"every namespace's policy matches a marketing run of a job now in finance, as before D4")
	for name, d := range got {
		require.Equal(t, evt, d.Event, "%s receives the event as built", name)
	}

	// Contract breaks are not scoped either.
	f.policy("pol-cb-fin", "finance", f.channel("ch-cb-fin", "finance", true), event.TypeContractBreakDeclared, "")
	f.job("producer-l", "marketing", nil)
	consumer := f.consumerJob("consumer-l", "marketing")
	payload := contractPayload(t, "consumer-l", [2]string{"producer-l", "consumer-l"})
	got = f.route(event.Event{Type: event.TypeContractBreakDeclared, JobID: consumer, Payload: payload})
	require.Equal(t, []string{"pol-cb-fin"}, policyNames(got))
	require.Equal(t, payload, got["pol-cb-fin"].Event.Payload)
}

// TestRouteEventAllDefaultIsByteIdentical: a deployment that keeps everything
// in default delivers what it always did, payload untouched.
func TestRouteEventAllDefaultIsByteIdentical(t *testing.T) {
	f := newRouteFixture(t)
	f.policies(event.TypeRunCompleted, "default")
	f.policy("pol-labels", "default", f.channel("ch-labels", "default", true), event.TypeRunCompleted, `{"labels":{"team":"data"}}`)
	job := f.job("legacy-job", "default", map[string]any{"team": "data"})
	// A legacy empty namespace reads as default on either side.
	require.NoError(t, f.db.Exec("UPDATE jobs SET namespace = '' WHERE id = ?", job).Error)
	evt := event.Event{Type: event.TypeRunCompleted, JobID: job, RunID: f.run(job, "default"), Payload: runPayload(t, "legacy-job", map[string]string{"team": "data"})}

	got := f.route(evt)
	require.Equal(t, []string{"pol-default", "pol-labels"}, policyNames(got))
	for _, d := range got {
		require.Equal(t, evt, d.Event, "the delivered event is the event, byte for byte")
	}
}

// TestRouteEventKeysOnTheRunsPersistedNamespace: a run that outlived its
// job's move alerts its old owner, without the new owner's labels.
func TestRouteEventKeysOnTheRunsPersistedNamespace(t *testing.T) {
	f := newRouteFixture(t)
	f.policies(event.TypeRunCompleted, "*", "marketing", "finance")
	f.policy("pol-mkt-labels", "marketing", f.channel("ch-mkt-labels", "marketing", true), event.TypeRunCompleted, `{"labels":{"owner":"finance-team"}}`)
	job := f.job("moved-job", "finance", map[string]any{"owner": "finance-team"})
	run := f.run(job, "marketing")
	evt := event.Event{Type: event.TypeRunCompleted, JobID: job, RunID: run,
		Payload: mustJSON(t, map[string]any{"job_alias": "moved-job", "job_labels": map[string]string{"owner": "finance-team"},
			"tasks": []map[string]any{{"name": "a", "job_labels": map[string]string{"owner": "finance-team"}}}})}

	got := f.route(evt)
	require.Equal(t, []string{"pol-*", "pol-marketing"}, policyNames(got),
		"the old owner is alerted, the new owner is not, and a label filter is never evaluated on the new owner's labels")
	require.Equal(t, evt, got["pol-*"].Event, "a shared destination receives the full event")
	scoped := buildPayload(got["pol-marketing"].Event)
	require.Nil(t, scoped.JobLabels)
	require.NotContains(t, string(scoped.RawPayload), "finance-team")
	require.Equal(t, "moved-job", scoped.JobAlias)
}

// TestRouteEventIncidentEventsBelongToTheIncident: an escalation for a
// marketing incident goes to marketing even when its job moved to finance and
// the event carries a finance remediation run.
func TestRouteEventIncidentEventsBelongToTheIncident(t *testing.T) {
	f := newRouteFixture(t)
	f.policies(event.TypeIncidentEscalated, "*", "marketing", "finance")
	job := f.job("escalating-job", "finance", map[string]any{"owner": "finance-team"})
	incident := f.incident(job, "marketing")
	retry := f.run(job, "finance")
	payload := mustJSON(t, map[string]any{"incident_id": incident.String(), "summary": "s", "job_alias": "escalating-job", "job_labels": map[string]string{"owner": "finance-team"}})

	got := f.route(event.Event{Type: event.TypeIncidentEscalated, JobID: job, RunID: retry, Payload: payload})
	require.Equal(t, []string{"pol-*", "pol-marketing"}, policyNames(got))
	require.NotContains(t, string(got["pol-marketing"].Event.Payload), "finance-team")

	// Runless: still the incident's.
	got = f.route(event.Event{Type: event.TypeIncidentEscalated, JobID: job, Payload: payload})
	require.Equal(t, []string{"pol-*", "pol-marketing"}, policyNames(got))
}

func TestRouteEventJobOnlyEventsUseTheJobNamespace(t *testing.T) {
	f := newRouteFixture(t)
	f.policies(event.TypeSLAMissed, "*", "marketing", "finance")
	job := f.job("sla-job", "finance", map[string]any{"team": "fin"})
	evt := event.Event{Type: event.TypeSLAMissed, JobID: job, Payload: runPayload(t, "sla-job", map[string]string{"team": "fin"})}

	got := f.route(evt)
	require.Equal(t, []string{"pol-*", "pol-finance"}, policyNames(got))
	require.Equal(t, evt, got["pol-finance"].Event, "a job-only event is not moved: its labels are its owner's")
}

func TestRouteEventUnresolvedOwnerReachesOnlySharedPolicies(t *testing.T) {
	f := newRouteFixture(t)
	f.policies(event.TypeRunCompleted, "*", "default", "marketing")
	job := f.job("gone-job", "default", nil)

	for name, evt := range map[string]event.Event{
		"unknown run":  {Type: event.TypeRunCompleted, JobID: job, RunID: uuid.New()},
		"unknown job":  {Type: event.TypeRunCompleted, JobID: uuid.New()},
		"no reference": {Type: event.TypeRunCompleted},
	} {
		require.Equal(t, []string{"pol-*"}, policyNames(f.route(evt)), name)
	}
	// Only an incident event is owned by the incident it names.
	require.Equal(t, []string{"pol-*", "pol-default"}, policyNames(f.route(event.Event{Type: event.TypeRunCompleted, JobID: job,
		Payload: mustJSON(t, map[string]string{"incident_id": uuid.NewString()})})), "incident_id on another type is not trusted")
	f.policy("pol-esc-default", "default", f.channel("ch-esc-default", "default", true), event.TypeIncidentEscalated, "")
	f.policy("pol-esc-shared", "*", f.channel("ch-esc-shared", "*", true), event.TypeIncidentEscalated, "")
	require.Equal(t, []string{"pol-esc-shared"}, policyNames(f.route(event.Event{Type: event.TypeIncidentEscalated, JobID: job,
		Payload: mustJSON(t, map[string]string{"incident_id": uuid.NewString()})})), "an escalation of an unknown incident reaches only shared policies")
	// A soft-deleted job still owns its job-only events: they stay with the
	// namespace it lived in (all-default deployments are byte-identical).
	require.NoError(t, f.db.Delete(&models.Job{}, "id = ?", job).Error)
	require.Equal(t, []string{"pol-*", "pol-default"}, policyNames(f.route(event.Event{Type: event.TypeRunCompleted, JobID: job})))

	// A lookup failure fails closed the same way (the runs table is gone).
	require.NoError(t, f.db.Migrator().DropTable(&models.JobRun{}))
	require.Equal(t, []string{"pol-*"}, policyNames(f.route(event.Event{Type: event.TypeRunCompleted, RunID: uuid.New()})))
}

// TestRouteEventChannelEligibility: the one decision delivery and the
// escalation routing report share. A policy whose channel is missing,
// disabled, or outside its namespace yields no delivery.
func TestRouteEventChannelEligibility(t *testing.T) {
	f := newRouteFixture(t)
	job := f.job("mkt-job", "marketing", nil)
	evt := event.Event{Type: event.TypeRunCompleted, JobID: job, RunID: f.run(job, "marketing")}

	f.policy("pol-foreign-channel", "marketing", f.channel("ch-fin", "finance", true), event.TypeRunCompleted, "")
	f.policy("pol-disabled-channel", "marketing", f.channel("ch-off", "marketing", false), event.TypeRunCompleted, "")
	f.policy("pol-missing-channel", "marketing", uuid.New(), event.TypeRunCompleted, "")
	deliveries, err := RouteEvent(context.Background(), f.db, evt)
	require.NoError(t, err)
	require.Empty(t, deliveries, "no eligible channel: nothing is delivered, and an escalation is not routed")

	f.policy("pol-shared-channel", "marketing", f.channel("ch-shared", "*", true), event.TypeRunCompleted, "")
	got := f.route(evt)
	require.Equal(t, []string{"pol-shared-channel"}, policyNames(got))
	require.Equal(t, "ch-shared", got["pol-shared-channel"].Channel.Name)
}

func contractPayload(t *testing.T, consumer string, findings ...[2]string) json.RawMessage {
	t.Helper()
	var list []map[string]any
	var impacted []map[string]string
	var keys []string
	for _, edge := range findings {
		// Team fields are read from the consumer's `team` label when the break
		// is declared; consumerJob gives each consumer that label. Each
		// consumer needs its own broken key (contractKey).
		list = append(list, map[string]any{"producer": edge[0], "consumer": edge[1], "consumer_team": edge[1] + "-team", "subject": "lake.orders",
			"dataset": "lake.orders", "output_key": contractKey(edge[1]), "detail": "removed " + contractKey(edge[1])})
		impacted = append(impacted, map[string]string{"alias": edge[1], "team": edge[1] + "-team"})
		keys = append(keys, contractKey(edge[1]))
	}
	slices.Sort(keys)
	return mustJSON(t, map[string]any{
		"job_alias": consumer, "consumer": consumer, "producer": findings[0][0], "dataset": "lake.orders", "offending_keys": keys,
		"impacted_consumers": impacted, "breaking_findings": list, "actor": "someone", "reason": "planned",
	})
}

// contractKey is the producer output key only consumer needs, spelled so it
// does not contain the consumer's alias.
func contractKey(consumer string) string {
	return strings.ReplaceAll(consumer, "-", "_") + "_col"
}

// consumerJob creates a contract consumer whose team label is the one
// contractPayload records for it.
func (f *routeFixture) consumerJob(alias, namespace string) uuid.UUID {
	return f.job(alias, namespace, map[string]any{"team": alias + "-team"})
}

func TestRouteEventScopesContractBreaksToTheDestinationNamespace(t *testing.T) {
	f := newRouteFixture(t)
	f.policies(event.TypeContractBreakDeclared, "*", "marketing", "finance")
	f.job("producer", "marketing", nil)
	c1 := f.consumerJob("consumer-mkt", "marketing")
	c2 := f.consumerJob("consumer-fin", "finance")
	payload := contractPayload(t, "consumer-mkt", [2]string{"producer", "consumer-mkt"}, [2]string{"producer", "consumer-fin"})

	got := f.route(event.Event{Type: event.TypeContractBreakDeclared, JobID: c1, Payload: payload})
	require.Equal(t, []string{"pol-*", "pol-marketing"}, policyNames(got))
	require.Equal(t, payload, got["pol-*"].Event.Payload, "a shared destination receives the whole break")
	require.Contains(t, string(payload), contractKey("consumer-fin"), "the unscoped summary names both consumers' broken keys")
	var scoped struct {
		Impacted      []map[string]string `json:"impacted_consumers"`
		Findings      []map[string]any    `json:"breaking_findings"`
		OffendingKeys []string            `json:"offending_keys"`
		Dataset       string              `json:"dataset"`
	}
	require.NoError(t, json.Unmarshal(got["pol-marketing"].Event.Payload, &scoped))
	require.Len(t, scoped.Findings, 1)
	require.Equal(t, "consumer-mkt", scoped.Findings[0]["consumer"])
	require.Equal(t, []map[string]string{{"alias": "consumer-mkt", "team": "consumer-mkt-team"}}, scoped.Impacted)
	require.Equal(t, []string{contractKey("consumer-mkt")}, scoped.OffendingKeys,
		"the summary's offending keys are recomputed from the kept findings")
	require.Equal(t, "lake.orders", scoped.Dataset)
	require.NotContains(t, string(got["pol-marketing"].Event.Payload), "consumer-fin")
	require.NotContains(t, string(got["pol-marketing"].Event.Payload), contractKey("consumer-fin"),
		"a key only the removed finance finding needs must not reach marketing")

	// The finance consumer's event: its only finding has a marketing
	// producer, so nothing about it may reach finance's destination.
	got = f.route(event.Event{Type: event.TypeContractBreakDeclared, JobID: c2,
		Payload: contractPayload(t, "consumer-fin", [2]string{"producer", "consumer-fin"}, [2]string{"producer", "consumer-mkt"})})
	require.Equal(t, []string{"pol-*"}, policyNames(got))

	// A break within one namespace is delivered untouched.
	f.consumerJob("solo-consumer", "marketing")
	solo := contractPayload(t, "solo-consumer", [2]string{"producer", "solo-consumer"})
	got = f.route(event.Event{Type: event.TypeContractBreakDeclared, JobID: c1, Payload: solo})
	require.Equal(t, solo, got["pol-marketing"].Event.Payload)

	// Another consumer that moved into marketing after the break was declared
	// still carries its old team in the event: the break is withheld from
	// marketing rather than delivered with that metadata.
	f.job("consumer-late", "marketing", map[string]any{"team": "marketing-team"})
	late := contractPayload(t, "consumer-mkt", [2]string{"producer", "consumer-mkt"}, [2]string{"producer", "consumer-late"})
	got = f.route(event.Event{Type: event.TypeContractBreakDeclared, JobID: c1, Payload: late})
	require.Equal(t, []string{"pol-*"}, policyNames(got))
	require.Equal(t, late, got["pol-*"].Event.Payload)
}

type recordingSender struct{ payloads []Payload }

func (r *recordingSender) Send(_ context.Context, _ models.NotificationChannel, payload Payload) error {
	r.payloads = append(r.payloads, payload)
	return nil
}

// TestSubscriberSendsTheScopedEvent: the subscriber delivers each
// destination's own view of the event.
func TestSubscriberSendsTheScopedEvent(t *testing.T) {
	f := newRouteFixture(t)
	f.policies(event.TypeRunCompleted, "marketing")
	job := f.job("moved", "finance", map[string]any{"owner": "finance-team"})
	sender := &recordingSender{}
	sub := NewSubscriber(nil, f.db)
	sub.RegisterSender(models.ChannelTypeWebhook, sender)

	sub.handleEvent(context.Background(), event.Event{Type: event.TypeRunCompleted, JobID: job, RunID: f.run(job, "marketing"),
		Payload: runPayload(t, "moved", map[string]string{"owner": "finance-team"})})
	require.Len(t, sender.payloads, 1)
	require.Nil(t, sender.payloads[0].JobLabels)
	require.NotContains(t, string(sender.payloads[0].RawPayload), "finance-team")
}

// TestRouteEventDatasetReleaseBelongsToTheBreachingRun: a release repeats the
// hold's evidence, which the run that opened it produced. After the producer
// moved, the release still goes to that run's namespace, not the job's new
// one.
func TestRouteEventDatasetReleaseBelongsToTheBreachingRun(t *testing.T) {
	f := newRouteFixture(t)
	f.policies(event.TypeDatasetReleased, "*", "marketing", "finance")
	job := f.job("moved-producer", "finance", nil)
	breaching := f.run(job, "marketing")
	hold := models.DatasetHold{ID: uuid.New(), Name: "lake.orders", Status: "released", HeldByJobID: job, HeldByRunID: &breaching}
	require.NoError(t, f.db.Create(&hold).Error)
	payload := mustJSON(t, map[string]any{"hold_id": hold.ID.String(), "dataset": "lake.orders", "status": "released",
		"violations": []map[string]any{{"assertion": "min", "observed": 1}}})

	got := f.route(event.Event{Type: event.TypeDatasetReleased, JobID: job, Payload: payload})
	require.Equal(t, []string{"pol-*", "pol-marketing"}, policyNames(got), "the breaching run's namespace owns the release")

	// A repeat breach after the move replaced the hold's evidence with the
	// new owner's: the hold now spans namespaces and its release reaches only
	// shared policies.
	finRun := f.run(job, "finance")
	require.NoError(t, f.db.Model(&models.DatasetHold{}).Where("id = ?", hold.ID).Update("last_breach_run_id", finRun).Error)
	got = f.route(event.Event{Type: event.TypeDatasetReleased, JobID: job, Payload: payload})
	require.Equal(t, []string{"pol-*"}, policyNames(got), "breaches in two namespaces: no single owner")
	// The latest breach in the opener's namespace keeps it there.
	mktRun := f.run(job, "marketing")
	require.NoError(t, f.db.Model(&models.DatasetHold{}).Where("id = ?", hold.ID).Update("last_breach_run_id", mktRun).Error)
	got = f.route(event.Event{Type: event.TypeDatasetReleased, JobID: job, Payload: payload})
	require.Equal(t, []string{"pol-*", "pol-marketing"}, policyNames(got))

	// A hold no run opened falls back to the job's namespace.
	manual := models.DatasetHold{ID: uuid.New(), Name: "lake.manual", Status: "released", HeldByJobID: job}
	require.NoError(t, f.db.Create(&manual).Error)
	got = f.route(event.Event{Type: event.TypeDatasetReleased, JobID: job, Payload: mustJSON(t, map[string]any{"hold_id": manual.ID.String()})})
	require.Equal(t, []string{"pol-*", "pol-finance"}, policyNames(got))

	// A release naming a hold that does not exist reaches only shared policies.
	got = f.route(event.Event{Type: event.TypeDatasetReleased, JobID: job, Payload: mustJSON(t, map[string]any{"hold_id": uuid.NewString()})})
	require.Equal(t, []string{"pol-*"}, policyNames(got))
}

// TestRouteEventWithholdsUnownedBaselineEvidence: a verdict recorded before
// baselines were namespace-owned (no baseline_scope) loses its baseline
// evidence at namespaced destinations, in a run's data_violations and in a
// dataset hold's stored violations; namespace-scoped verdicts and shared
// destinations are untouched.
func TestRouteEventWithholdsUnownedBaselineEvidence(t *testing.T) {
	f := newRouteFixture(t)
	f.policies(event.TypeRunFailed, "*", "marketing")
	job := f.job("verdict-job", "marketing", nil)
	run := f.run(job, "marketing")
	legacy := map[string]any{"assertion": "deltaFromBaseline", "observed": 5, "bound": 9, "baseline_median": 10, "baseline_samples": 7,
		"message": "row_count 5 is 50% below the baseline median 10"}
	scoped := map[string]any{"assertion": "min", "observed": 1, "bound": 2, "baseline_scope": "namespace", "baseline_median": 3, "message": "own"}
	evt := event.Event{Type: event.TypeRunFailed, JobID: job, RunID: run, Payload: mustJSON(t, map[string]any{
		"job_alias": "verdict-job", "data_violations": []map[string]any{legacy, scoped},
		"error": "data assertion failed: row_count 5 is 50% below the baseline median 10"})}

	got := f.route(evt)
	require.Equal(t, evt, got["pol-*"].Event, "a shared destination receives the event as recorded")
	body := string(got["pol-marketing"].Event.Payload)
	require.NotContains(t, body, "baseline median 10")
	require.NotContains(t, body, `"baseline_samples":7`)
	require.Contains(t, body, runstorage.BaselineEvidenceWithheld)
	require.Contains(t, body, `"message":"own"`, "a namespace-scoped verdict passes unchanged")

	// Only scoped verdicts: delivered byte for byte.
	clean := event.Event{Type: event.TypeRunFailed, JobID: job, RunID: run, Payload: mustJSON(t, map[string]any{
		"job_alias": "verdict-job", "data_violations": []map[string]any{scoped}})}
	require.Equal(t, clean, f.route(clean)["pol-marketing"].Event)

	// A dataset hold's stored violations are withheld the same way.
	f.policies(event.TypeDatasetReleased, "finance")
	finJob := f.job("verdict-producer", "finance", nil)
	finRun := f.run(finJob, "finance")
	hold := models.DatasetHold{ID: uuid.New(), Name: "lake.verdicts", Status: "released", HeldByJobID: finJob, HeldByRunID: &finRun}
	require.NoError(t, f.db.Create(&hold).Error)
	release := event.Event{Type: event.TypeDatasetReleased, JobID: finJob, Payload: mustJSON(t, map[string]any{
		"hold_id": hold.ID.String(), "violations": []map[string]any{legacy}})}
	got = f.route(release)
	require.Equal(t, []string{"pol-finance"}, policyNames(got))
	require.NotContains(t, string(got["pol-finance"].Event.Payload), "baseline median 10")
	require.Contains(t, string(got["pol-finance"].Event.Payload), runstorage.BaselineEvidenceWithheld)
}

// TestRouteEventLabelsMustBeTheJobsCurrentOnes: the job's namespace at
// dispatch cannot prove where already-built labels came from (the job can
// move away and back before dispatch). Labels reach a namespaced destination
// only when they are exactly the job's current labels in that namespace.
func TestRouteEventLabelsMustBeTheJobsCurrentOnes(t *testing.T) {
	f := newRouteFixture(t)
	f.policies(event.TypeRunCompleted, "*", "marketing", "finance")
	f.policy("pol-mkt-labels", "marketing", f.channel("ch-mkt-labels", "marketing", true), event.TypeRunCompleted, `{"labels":{"owner":"finance-team"}}`)
	job := f.job("returned-job", "marketing", map[string]any{"owner": "marketing-team"})
	run := f.run(job, "marketing")
	foreign := event.Event{Type: event.TypeRunCompleted, JobID: job, RunID: run,
		Payload: runPayload(t, "returned-job", map[string]string{"owner": "finance-team"})}

	got := f.route(foreign)
	require.Equal(t, []string{"pol-*", "pol-marketing"}, policyNames(got), "a label filter is never evaluated on labels the job no longer has")
	require.Equal(t, foreign, got["pol-*"].Event)
	require.NotContains(t, string(got["pol-marketing"].Event.Payload), "finance-team")

	// The job's current labels are delivered unchanged.
	own := event.Event{Type: event.TypeRunCompleted, JobID: job, RunID: run,
		Payload: runPayload(t, "returned-job", map[string]string{"owner": "marketing-team"})}
	got = f.route(own)
	require.Equal(t, own, got["pol-marketing"].Event)

	// A job-only event is checked the same way.
	f.policy("pol-sla-mkt", "marketing", f.channel("ch-sla-mkt", "marketing", true), event.TypeSLAMissed, "")
	sla := event.Event{Type: event.TypeSLAMissed, JobID: job, Payload: runPayload(t, "returned-job", map[string]string{"owner": "finance-team"})}
	got = f.route(sla)
	require.Equal(t, []string{"pol-sla-mkt"}, policyNames(got))
	require.NotContains(t, string(got["pol-sla-mkt"].Event.Payload), "finance-team")

	// A contract break whose labels are not the consumer's current ones is
	// withheld from namespaced destinations: its team fields come from them.
	f.policy("pol-cb-mkt", "marketing", f.channel("ch-cb-mkt", "marketing", true), event.TypeContractBreakDeclared, "")
	f.policy("pol-cb-shared", "*", f.channel("ch-cb-shared", "*", true), event.TypeContractBreakDeclared, "")
	f.job("producer-x", "marketing", nil)
	consumer := f.consumerJob("consumer-x", "marketing")
	var fields map[string]any
	require.NoError(t, json.Unmarshal(contractPayload(t, "consumer-x", [2]string{"producer-x", "consumer-x"}), &fields))
	fields["job_labels"] = map[string]string{"team": "finance-team"}
	got = f.route(event.Event{Type: event.TypeContractBreakDeclared, JobID: consumer, Payload: mustJSON(t, fields)})
	require.Equal(t, []string{"pol-cb-shared"}, policyNames(got))
	fields["job_labels"] = map[string]string{"team": "consumer-x-team"}
	got = f.route(event.Event{Type: event.TypeContractBreakDeclared, JobID: consumer, Payload: mustJSON(t, fields)})
	require.Equal(t, []string{"pol-cb-mkt", "pol-cb-shared"}, policyNames(got))
}
