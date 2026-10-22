package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// Routing is namespace-aware (identity-and-access D4, design §10.4) while a
// policy file is loaded: a notification policy delivers only the events its
// own namespace owns, and a cluster-shared policy (namespace "*") delivers
// every event. With no policy file (design §§1/15: behaviour unchanged) every
// policy matches by event type and filters and receives the event as built,
// as before D4. In both modes a delivery needs an existing, enabled channel
// visible from the policy (the create and patch rule, so no reachable row is
// affected).
//
// An event's owner is the persisted ownership of what it is about, never the
// job's current namespace when something more specific exists:
//
//  1. an incident event (incident_escalated, agent_action_executed; the
//     payload's incident_id) belongs to the incident's persisted namespace —
//     the run it carries may be a remediation retry launched after the job
//     moved;
//  2. a dataset_released event (the payload's hold_id) belongs to the
//     persisted namespace of the runs whose evidence it repeats, the one that
//     opened the hold and the one of its latest breach (when those differ in
//     namespace, it has no owner);
//  3. otherwise an event naming a run belongs to the run's persisted
//     namespace;
//  4. otherwise (sla_missed without a run, contract_break_declared, a release
//     of a hold no run opened) it belongs to the job's namespace, the
//     namespace it was deleted from for a deleted job.
//
// Every notifiable type names a run, an incident or a job. An event whose
// owner cannot be resolved — it names none, the row is gone, or the lookup
// failed — reaches only "*" policies.
//
// A non-"*" destination also receives a payload scoped to its namespace. The
// job labels an event carries were read when it was built; unless the job
// lives in the owner's namespace now and still has exactly those labels (a
// run or incident can outlive a move, and a job can move away and back before
// dispatch), they are removed, a label filter does not match, and a contract
// break is not delivered. A contract break otherwise keeps only the findings
// between jobs of that namespace. Data verdicts recorded before baselines
// were namespace-owned lose their baseline evidence.

// Delivery is one destination of an event: a matching policy, its channel,
// and the event as that destination may see it.
type Delivery struct {
	Policy  models.NotificationPolicy
	Channel models.NotificationChannel
	Event   event.Event
}

// RouteEvent returns the deliveries for evt: every enabled policy whose event
// types, ownership and filters match it, whose channel exists, is enabled and
// is visible from the policy's namespace.
//
// It is the one routing decision: the subscriber sends exactly these, and an
// escalation reports itself routed exactly when there is one
// (cmd/start/incident_ops.go Escalate), so the two cannot disagree.
func RouteEvent(ctx context.Context, db *gorm.DB, evt event.Event) ([]Delivery, error) {
	var candidates []models.NotificationPolicy
	// Filter at the SQL level: only load policies whose event_types JSON
	// contains the event type string. This is a substring match on the
	// JSON column — not exact, but it eliminates the vast majority of
	// non-matching rows. The in-memory policyMatchesEvent check below
	// is the authoritative filter.
	if err := db.WithContext(ctx).
		Where("enabled = ? AND event_types LIKE ?", true, "%"+string(evt.Type)+"%").
		Find(&candidates).Error; err != nil {
		return nil, err
	}

	// Namespace isolation needs a policy file (design §§1/15: with none,
	// behaviour is unchanged), read at the point of use as B2's declared
	// namespaces are.
	router := eventRouter{ctx: ctx, db: db, evt: evt, isolated: policy.ProcessHolder().FileBacked()}
	var matched []Delivery
	for _, p := range candidates {
		if !policyMatchesEvent(p, evt) {
			continue
		}
		delivered, ok := router.scopeFor(p)
		if !ok {
			continue
		}
		if !policyFilterMatches(p, evt) {
			continue
		}
		matched = append(matched, Delivery{Policy: p, Event: delivered})
	}
	if len(matched) == 0 {
		return nil, nil
	}

	channels, err := loadPolicyChannels(ctx, db, matched)
	if err != nil {
		return nil, err
	}
	deliveries := matched[:0]
	for _, d := range matched {
		channel, ok := channels[d.Policy.ChannelID]
		if !ok {
			log.Error("notification: channel not found",
				"channel_id", d.Policy.ChannelID,
			)
			continue
		}
		if !channel.Enabled {
			continue
		}
		// A policy references only a channel in its own namespace or "*"
		// (enforced on create and patch); a row that does not is never
		// delivered through.
		if !models.ObjectVisibleFrom(d.Policy.Namespace, channel.Namespace) {
			log.Warn("notification: policy references a channel outside its namespace; not delivering",
				"policy_id", d.Policy.ID,
				"channel_id", channel.ID,
			)
			continue
		}
		d.Channel = channel
		deliveries = append(deliveries, d)
	}
	return deliveries, nil
}

// loadPolicyChannels fetches every channel the deliveries reference in one
// query, indexed by ID.
func loadPolicyChannels(ctx context.Context, db *gorm.DB, deliveries []Delivery) (map[uuid.UUID]models.NotificationChannel, error) {
	ids := make([]uuid.UUID, 0, len(deliveries))
	seen := make(map[uuid.UUID]struct{}, len(deliveries))
	for _, d := range deliveries {
		if _, dup := seen[d.Policy.ChannelID]; !dup {
			ids = append(ids, d.Policy.ChannelID)
			seen[d.Policy.ChannelID] = struct{}{}
		}
	}

	var channels []models.NotificationChannel
	if err := db.WithContext(ctx).
		Where("id IN ?", ids).
		Find(&channels).Error; err != nil {
		return nil, err
	}

	m := make(map[uuid.UUID]models.NotificationChannel, len(channels))
	for _, ch := range channels {
		m[ch.ID] = ch
	}
	return m, nil
}

// eventRouter resolves one event's ownership lazily (only when a non-"*"
// policy is a candidate) and at most once.
type eventRouter struct {
	ctx context.Context
	db  *gorm.DB
	evt event.Event
	// isolated: a policy file is loaded, so policies match only the events
	// their namespace owns. Without one every policy matches as it always
	// did, and receives the event unchanged.
	isolated bool

	owner    *eventOwner
	contract map[string]contractScope // per destination namespace
}

type eventOwner struct {
	namespace string
	resolved  bool
	// moved: the job metadata the event carries cannot be shown to be the
	// owner's — the job's current namespace is not the owner's (or unknown),
	// or the labels the event carries are not the job's current ones.
	moved bool
}

type contractScope struct {
	payload json.RawMessage
	ok      bool
}

// scopeFor decides whether policy p may receive the event and returns the
// event as p's destination may see it.
func (r *eventRouter) scopeFor(p models.NotificationPolicy) (event.Event, bool) {
	if p.Namespace == models.ClusterNamespace || !r.isolated {
		return r.evt, true
	}
	owner := r.resolveOwner()
	if !owner.resolved || !models.ObjectVisibleFrom(owner.namespace, p.Namespace) {
		return event.Event{}, false
	}
	delivered := r.evt
	if owner.moved {
		if delivered.Type == event.TypeContractBreakDeclared {
			// Its consumer team fields are read from the same labels.
			return event.Event{}, false
		}
		if policyFiltersOnLabels(p) {
			// The labels the event carries are the job's current ones, which
			// may belong to another namespace: never evaluate them.
			return event.Event{}, false
		}
		payload, ok := withoutJobLabels(delivered.Payload)
		if !ok {
			return event.Event{}, false
		}
		delivered.Payload = payload
	}
	payload, ok := withoutUnownedBaselineEvidence(delivered.Payload)
	if !ok {
		return event.Event{}, false
	}
	delivered.Payload = payload
	if delivered.Type == event.TypeContractBreakDeclared {
		payload, ok := r.contractPayload(delivered.Payload, models.NamespaceOrDefault(p.Namespace))
		if !ok {
			return event.Event{}, false
		}
		delivered.Payload = payload
	}
	return delivered, true
}

func (r *eventRouter) resolveOwner() eventOwner {
	if r.owner != nil {
		return *r.owner
	}
	owner := resolveEventOwner(r.ctx, r.db, r.evt)
	r.owner = &owner
	return owner
}

// resolveEventOwner applies the ownership rules above. Any failure leaves the
// owner unresolved.
func resolveEventOwner(ctx context.Context, db *gorm.DB, evt event.Event) eventOwner {
	namespace, err := ownerNamespace(ctx, db, evt)
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) && !errors.Is(err, errNoOwnerReference) {
			log.Warn("notification: could not resolve the event's owning namespace; delivering to shared policies only",
				"event_type", string(evt.Type),
				"run_id", evt.RunID,
				"job_id", evt.JobID,
				"error", err,
			)
		}
		return eventOwner{}
	}
	owner := eventOwner{namespace: namespace, resolved: true, moved: true}
	if evt.JobID != uuid.Nil {
		// The payload's job metadata is the owner's only when the job lives in
		// the owner's namespace now AND the labels it carries are the job's
		// current ones: the namespace alone cannot prove where already-built
		// labels came from (a job can move away and back before dispatch).
		// Unscoped: a deleted job's last namespace and labels still count.
		current, labels, err := lookupJobOwnership(ctx, db.Unscoped(), evt.JobID)
		owner.moved = err != nil || current != namespace || !payloadLabelsAre(evt.Payload, labels)
	}
	return owner
}

// lookupJobOwnership reads a job's namespace and labels.
func lookupJobOwnership(ctx context.Context, db *gorm.DB, id uuid.UUID) (string, map[string]any, error) {
	var job struct {
		Namespace string
		Labels    datatypes.JSONMap
	}
	if err := db.WithContext(ctx).Model(&models.Job{}).Select("namespace", "labels").Where("id = ?", id).Take(&job).Error; err != nil {
		return "", nil, err
	}
	return models.NamespaceOrDefault(job.Namespace), map[string]any(job.Labels), nil
}

// payloadLabelsAre reports whether every job_labels object the payload
// carries (top level and on each tasks[] entry) is exactly labels. A payload
// that carries no labels has none to disown; one that names labels but is not
// a JSON object does not match.
func payloadLabelsAre(payload json.RawMessage, labels map[string]any) bool {
	if len(payload) == 0 || !bytes.Contains(payload, []byte(`"job_labels"`)) {
		return true
	}
	fields, ok := decodeObject(payload)
	if !ok {
		return false
	}
	want, err := canonicalLabels(labels)
	if err != nil {
		return false
	}
	same := func(value any) bool {
		got, err := canonicalLabels(value)
		return err == nil && got == want
	}
	if value, present := fields["job_labels"]; present && !same(value) {
		return false
	}
	if tasks, ok := fields["tasks"].([]any); ok {
		for _, task := range tasks {
			if entry, ok := task.(map[string]any); ok {
				if value, present := entry["job_labels"]; present && !same(value) {
					return false
				}
			}
		}
	}
	return true
}

// canonicalLabels encodes a labels value for comparison (encoding/json sorts
// map keys); an absent or empty set encodes as "{}".
func canonicalLabels(value any) (string, error) {
	if value == nil {
		return "{}", nil
	}
	if labels, ok := value.(map[string]any); ok && len(labels) == 0 {
		return "{}", nil
	}
	encoded, err := json.Marshal(value)
	return string(encoded), err
}

var errNoOwnerReference = errors.New("event names no run, incident or job")

// ownerNamespace returns the event's owning namespace.
func ownerNamespace(ctx context.Context, db *gorm.DB, evt event.Event) (string, error) {
	if incidentID, ok := payloadID(evt, incidentEventTypes, "incident_id"); ok {
		return lookupNamespace(ctx, db, &models.Incident{}, incidentID)
	}
	if holdID, ok := payloadID(evt, holdEventTypes, "hold_id"); ok {
		// The hold's evidence is its breaching runs': its release belongs to
		// their namespace even after the producer moved.
		namespace, found, err := holdNamespace(ctx, db, holdID)
		if err != nil {
			return "", err
		}
		if found {
			return namespace, nil
		}
	}
	if evt.RunID != uuid.Nil {
		return lookupNamespace(ctx, db, &models.JobRun{}, evt.RunID)
	}
	if evt.JobID != uuid.Nil {
		// Unscoped: a job-only event about a since-deleted job still belongs
		// to the namespace that job lived in.
		return lookupNamespace(ctx, db.Unscoped(), &models.Job{}, evt.JobID)
	}
	return "", errNoOwnerReference
}

// errMixedHoldOwnership: a hold's breaching runs live in different
// namespaces (its producer moved between breaches), so its evidence has no
// single owner.
var errMixedHoldOwnership = errors.New("dataset hold breaches span namespaces")

// holdNamespace returns the namespace of the runs whose evidence a dataset
// hold carries: the run that opened it and the run of its latest breach,
// whose violations replace the opening ones (found=false when no run did
// either). Runs in different namespaces are errMixedHoldOwnership.
func holdNamespace(ctx context.Context, db *gorm.DB, holdID uuid.UUID) (string, bool, error) {
	var hold models.DatasetHold
	if err := db.WithContext(ctx).Select("id", "held_by_run_id", "last_breach_run_id").Where("id = ?", holdID).Take(&hold).Error; err != nil {
		return "", false, err
	}
	namespace := ""
	for _, runID := range []*uuid.UUID{hold.HeldByRunID, hold.LastBreachRunID} {
		if runID == nil || *runID == uuid.Nil {
			continue
		}
		runNamespace, err := lookupNamespace(ctx, db, &models.JobRun{}, *runID)
		if err != nil {
			return "", false, err
		}
		if namespace != "" && runNamespace != namespace {
			return "", false, errMixedHoldOwnership
		}
		namespace = runNamespace
	}
	return namespace, namespace != "", nil
}

// lookupNamespace reads one row's namespace by id; gorm.ErrRecordNotFound
// when there is no (live) row.
func lookupNamespace(ctx context.Context, db *gorm.DB, model any, id uuid.UUID) (string, error) {
	var namespaces []string
	if err := db.WithContext(ctx).Model(model).Where("id = ?", id).Limit(1).Pluck("namespace", &namespaces).Error; err != nil {
		return "", err
	}
	if len(namespaces) == 0 {
		return "", gorm.ErrRecordNotFound
	}
	return models.NamespaceOrDefault(namespaces[0]), nil
}

// incidentEventTypes are the notifiable types whose payload names the
// incident they are about (incident_id, set by the server). Only these are
// owned by the incident: the key is not trusted on any other type.
var incidentEventTypes = map[event.Type]bool{
	event.TypeIncidentEscalated:   true,
	event.TypeAgentActionExecuted: true,
}

// holdEventTypes are the notifiable types owned by the run that opened the
// dataset hold their payload names (hold_id, set by the server). A held event
// already carries that run; a release does not.
var holdEventTypes = map[event.Type]bool{
	event.TypeDatasetReleased: true,
}

// payloadID returns the id an event of one of types names under key.
func payloadID(evt event.Event, types map[event.Type]bool, key string) (uuid.UUID, bool) {
	payload := evt.Payload
	if !types[evt.Type] || len(payload) == 0 || !bytes.Contains(payload, []byte(`"`+key+`"`)) {
		return uuid.Nil, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return uuid.Nil, false
	}
	var raw string
	if err := json.Unmarshal(fields[key], &raw); err != nil {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil || id == uuid.Nil {
		return uuid.Nil, false
	}
	return id, true
}

// policyFiltersOnLabels reports whether p filters on job labels; an
// undecodable filter counts (it never matches anyway).
func policyFiltersOnLabels(p models.NotificationPolicy) bool {
	if len(p.Filters) == 0 {
		return false
	}
	var filter PolicyFilter
	if err := json.Unmarshal(p.Filters, &filter); err != nil {
		return true
	}
	return len(filter.Labels) > 0
}

// withoutJobLabels removes the job's labels from an event payload, at the top
// level and on each tasks[] entry (as the event stream does for confined
// subscribers, api/rest/controller/event/stream.go redactStreamPayload). A
// payload that names labels but is not a JSON object is withheld.
func withoutJobLabels(payload json.RawMessage) (json.RawMessage, bool) {
	if len(payload) == 0 || !bytes.Contains(payload, []byte(`"job_labels"`)) {
		return payload, true
	}
	fields, ok := decodeObject(payload)
	if !ok {
		return nil, false
	}
	delete(fields, "job_labels")
	if tasks, ok := fields["tasks"].([]any); ok {
		for _, task := range tasks {
			if entry, ok := task.(map[string]any); ok {
				delete(entry, "job_labels")
			}
		}
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

// withoutUnownedBaselineEvidence withholds, from the data verdicts a payload
// carries (data_violations at the top level and on tasks[], and a dataset
// hold's violations), the baseline evidence of every verdict not computed over
// its own run's namespace: rows recorded before baselines were namespace-owned
// (run.WithholdUnownedBaselineEvidenceJSON, as confined run and event reads
// do). Such rows still reach dispatch, through a hold's stored violations or
// a persisted event dispatched late. A payload with nothing to withhold is
// returned unchanged; one that names baseline evidence but is not a JSON
// object is withheld.
func withoutUnownedBaselineEvidence(payload json.RawMessage) (json.RawMessage, bool) {
	if len(payload) == 0 ||
		(!bytes.Contains(payload, []byte(`"baseline_median"`)) &&
			!bytes.Contains(payload, []byte(`"baseline_samples"`)) &&
			!bytes.Contains(payload, []byte(runstorage.AssertionDeltaFromBaseline))) {
		return payload, true
	}
	before, ok := decodeObject(payload)
	if !ok {
		return nil, false
	}
	after, _ := decodeObject(payload)
	if violations, present := after["violations"]; present {
		wrapper := map[string]any{"data_violations": violations}
		runstorage.WithholdUnownedBaselineEvidenceJSON(wrapper)
		if kept, ok := wrapper["data_violations"]; ok {
			after["violations"] = kept
		} else {
			delete(after, "violations")
		}
	}
	runstorage.WithholdUnownedBaselineEvidenceJSON(after)
	original, err := json.Marshal(before)
	if err != nil {
		return nil, false
	}
	redacted, err := json.Marshal(after)
	if err != nil {
		return nil, false
	}
	if bytes.Equal(original, redacted) {
		return payload, true
	}
	return redacted, true
}

func decodeObject(payload json.RawMessage) (map[string]any, bool) {
	var fields map[string]any
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber() // keep every other value numerically byte-for-byte
	if err := decoder.Decode(&fields); err != nil || fields == nil {
		return nil, false
	}
	return fields, true
}

// contractPayload scopes a contract_break_declared payload for a destination
// in namespace, caching the answer per namespace.
func (r *eventRouter) contractPayload(payload json.RawMessage, namespace string) (json.RawMessage, bool) {
	if scope, ok := r.contract[namespace]; ok {
		return scope.payload, scope.ok
	}
	scoped, ok := scopeContractBreak(r.ctx, r.db, payload, namespace)
	if r.contract == nil {
		r.contract = map[string]contractScope{}
	}
	r.contract[namespace] = contractScope{payload: scoped, ok: ok}
	return scoped, ok
}

// scopeContractBreak keeps, of a contract break's findings, only those
// between jobs of namespace (consumer and producer both there), and of its
// impacted consumers only that namespace's. A break of a producer in another
// namespace leaves nothing, and is not delivered: its findings, subject and
// acknowledgement name the producer and its namespace's other consumers
// (design §18: cross-namespace aggregates never reveal another namespace's
// aliases). An alias that resolves to no live job counts as foreign. When
// nothing is removed the payload is returned unchanged.
func scopeContractBreak(ctx context.Context, db *gorm.DB, payload json.RawMessage, namespace string) (json.RawMessage, bool) {
	fields, ok := decodeObject(payload)
	if !ok {
		return nil, false
	}
	findings, _ := fields["breaking_findings"].([]any)
	impacted, _ := fields["impacted_consumers"].([]any)
	// finding_contributors (internal/contract emitContractBreakDeclaredEvents)
	// is present only when colliding edge ids merged another pair's findings
	// into an edge: aligned with breaking_findings, it lists the job aliases of
	// every edge merged into each finding, all of which must be the
	// destination's (identity-and-access W7-ε). Malformed: deliver nothing.
	rawContributors, hasContributors := fields["finding_contributors"]
	var contributors []any
	if hasContributors {
		list, ok := rawContributors.([]any)
		if !ok || len(list) != len(findings) {
			return nil, false
		}
		contributors = list
	}
	contributorAliases := func(i int) ([]string, bool) {
		if !hasContributors {
			return nil, true
		}
		list, ok := contributors[i].([]any)
		if !ok {
			return nil, false
		}
		out := make([]string, 0, len(list))
		for _, value := range list {
			alias, ok := value.(string)
			if !ok {
				return nil, false
			}
			out = append(out, alias)
		}
		return out, true
	}

	aliasSet := map[string]struct{}{}
	add := func(value any) {
		if alias, ok := value.(string); ok && strings.TrimSpace(alias) != "" {
			aliasSet[strings.TrimSpace(alias)] = struct{}{}
		}
	}
	add(fields["producer"])
	add(fields["consumer"])
	for _, entry := range findings {
		if finding, ok := entry.(map[string]any); ok {
			add(finding["producer"])
			add(finding["consumer"])
		}
	}
	for _, entry := range impacted {
		if consumer, ok := entry.(map[string]any); ok {
			add(consumer["alias"])
		}
	}
	for i := range contributors {
		list, _ := contributorAliases(i)
		for _, alias := range list {
			add(alias)
		}
	}
	aliases := make([]string, 0, len(aliasSet))
	for alias := range aliasSet {
		aliases = append(aliases, alias)
	}

	var jobs []struct {
		Alias     string
		Namespace string
		Labels    datatypes.JSONMap
	}
	if len(aliases) > 0 {
		if err := db.WithContext(ctx).Model(&models.Job{}).Select("alias", "namespace", "labels").Where("alias IN ?", aliases).Find(&jobs).Error; err != nil {
			log.Warn("notification: could not resolve contract break jobs; delivering to shared policies only", "error", err)
			return nil, false
		}
	}
	namespaceOf := make(map[string]string, len(jobs))
	teamOf := make(map[string]string, len(jobs))
	for _, job := range jobs {
		namespaceOf[job.Alias] = models.NamespaceOrDefault(job.Namespace)
		team, _ := job.Labels["team"].(string)
		teamOf[job.Alias] = strings.TrimSpace(team)
	}
	// currentTeam reports whether a retained consumer's team metadata (read
	// from its labels when the break was declared) is still that consumer's
	// team: a consumer that moved into the namespace after the event was built
	// must not bring its old namespace's team along.
	currentTeam := func(alias, team any) bool {
		name, _ := alias.(string)
		value, _ := team.(string)
		return strings.TrimSpace(value) == teamOf[strings.TrimSpace(name)]
	}
	inNamespace := func(value any) bool {
		alias, ok := value.(string)
		if !ok {
			return false
		}
		ns, found := namespaceOf[strings.TrimSpace(alias)]
		return found && ns == namespace
	}

	if !inNamespace(fields["producer"]) {
		return nil, false
	}
	if consumer, present := fields["consumer"]; present && !inNamespace(consumer) {
		return nil, false
	}
	contributorsInNamespace := func(i int) bool {
		list, ok := contributorAliases(i)
		if !ok {
			return false
		}
		for _, alias := range list {
			if !inNamespace(alias) {
				return false
			}
		}
		return true
	}
	keptFindings := make([]any, 0, len(findings))
	for i, entry := range findings {
		finding, ok := entry.(map[string]any)
		if ok && inNamespace(finding["consumer"]) && inNamespace(finding["producer"]) && contributorsInNamespace(i) {
			keptFindings = append(keptFindings, entry)
		}
	}
	if len(keptFindings) == 0 {
		return nil, false
	}
	keptImpacted := make([]any, 0, len(impacted))
	for _, entry := range impacted {
		if consumer, ok := entry.(map[string]any); ok && inNamespace(consumer["alias"]) {
			keptImpacted = append(keptImpacted, entry)
		}
	}
	// Team metadata that is not the retained consumer's current team cannot
	// be shown to be this namespace's: withhold the break rather than send it.
	for _, entry := range keptFindings {
		finding := entry.(map[string]any)
		if !currentTeam(finding["consumer"], finding["consumer_team"]) {
			return nil, false
		}
	}
	for _, entry := range keptImpacted {
		consumer := entry.(map[string]any)
		if !currentTeam(consumer["alias"], consumer["team"]) {
			return nil, false
		}
	}
	if !hasContributors && len(keptFindings) == len(findings) && len(keptImpacted) == len(impacted) {
		return payload, true
	}
	// The contributor list names every merged pair's jobs; a scoped payload
	// never carries it.
	delete(fields, "finding_contributors")
	fields["breaking_findings"] = keptFindings
	fields["impacted_consumers"] = keptImpacted
	// The summary fields the emitter derives from the findings
	// (internal/contract emitContractBreakDeclaredEvents: offendingKeys,
	// datasetForGroup) are recomputed from the kept ones, so a removed
	// finding's broken keys do not survive in the summary.
	if _, present := fields["offending_keys"]; present {
		fields["offending_keys"] = findingOutputKeys(keptFindings)
	}
	if _, present := fields["dataset"]; present {
		fields["dataset"] = findingDataset(keptFindings)
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

// findingOutputKeys is the sorted, de-duplicated output_key set of findings
// (internal/contract offendingKeys).
func findingOutputKeys(findings []any) []string {
	seen := map[string]struct{}{}
	for _, entry := range findings {
		finding, _ := entry.(map[string]any)
		if key, _ := finding["output_key"].(string); strings.TrimSpace(key) != "" {
			seen[strings.TrimSpace(key)] = struct{}{}
		}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// findingDataset is the display name of the first finding's dataset
// (internal/contract displayDatasetName(datasetForGroup)).
func findingDataset(findings []any) string {
	for _, entry := range findings {
		finding, _ := entry.(map[string]any)
		dataset, _ := finding["dataset"].(string)
		if dataset = strings.TrimSpace(dataset); dataset != "" {
			if strings.HasPrefix(dataset, "/") && !strings.Contains(strings.TrimPrefix(dataset, "/"), "/") {
				return strings.TrimPrefix(dataset, "/")
			}
			return dataset
		}
	}
	return ""
}
