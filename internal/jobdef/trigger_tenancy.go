package jobdef

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Trigger tenancy (identity-and-access design §9.2, plan item B9).
//
// A trigger row serves every live job whose trigger_id names it, and a webhook
// delivery fans out to every HTTP trigger on its normalized path. Two rules
// keep namespaces apart, both evaluated on the state a write proposes, before
// it writes anything:
//
//  1. A confined writer (a non-nil TenancyScope: a namespace-confined
//     principal, a job-scoped key, or an allowlisted git-sync source) may
//     never write a job whose trigger row also serves, or whose webhook path
//     is also subscribed by, a live job outside its scope. Always enforced.
//  2. With a policy file loaded, every write must leave each trigger row
//     serving one namespace and each webhook path subscribed from one
//     namespace. A writer authorised over every job a shared row serves
//     repairs the row by detaching the written job onto a fresh trigger row
//     (the old row, and its history, stay with the other jobs); a webhook path
//     subscribed from another namespace is refused.
//
// No API path makes two jobs share a trigger row (the importer reuses only a
// job's own row and the legacy create always makes a new one), so shared rows
// are legacy data; webhook paths are shared routinely (fan-out).

// ErrTriggerTenancy is wrapped by *TriggerTenancyError.
var ErrTriggerTenancy = errors.New("trigger tenancy conflict")

// Reasons a *NamespaceAllowlistError reports for a tenancy refusal.
const (
	NamespaceReasonTriggerShared = "trigger_serves_job_outside_scope"
	NamespaceReasonWebhookPath   = "webhook_path_subscribed_outside_scope"
)

// TenancyScope confines a write (or, for a manifest export, a read) to
// namespaces and job aliases. A nil *TenancyScope is unrestricted.
type TenancyScope struct {
	// Namespaces the writer may act in; nil allows every namespace.
	Namespaces []string
	// JobAliases the writer's key scope names; nil allows every alias.
	JobAliases []string
}

// NewTenancyScope returns the scope for the given restrictions, or nil when
// neither restricts anything.
func NewTenancyScope(namespaces, jobAliases []string) *TenancyScope {
	if namespaces == nil && jobAliases == nil {
		return nil
	}
	return &TenancyScope{Namespaces: slices.Clone(namespaces), JobAliases: slices.Clone(jobAliases)}
}

// Allows reports whether the job alias in namespace is inside the scope.
func (s *TenancyScope) Allows(alias, namespace string) bool {
	if s == nil {
		return true
	}
	if s.Namespaces != nil && !slices.Contains(s.Namespaces, models.NamespaceOrDefault(strings.TrimSpace(namespace))) {
		return false
	}
	if s.JobAliases != nil && !slices.Contains(s.JobAliases, strings.TrimSpace(alias)) {
		return false
	}
	return true
}

// TriggerTenancyError is a rule-2 violation: the write would leave a webhook
// path subscribed from more than one namespace or, for a trigger PATCH, a row
// serving more than one. Its message names the namespaces, so only a writer
// that may read them (an unrestricted one) is shown it.
type TriggerTenancyError struct {
	Alias       string    // the job written; "" for a trigger PATCH
	TriggerID   uuid.UUID // set for a trigger PATCH
	WebhookPath string    // "" for a row conflict
	Namespace   string    // the namespace the write proposes ("" for a row conflict)
	Others      []string  // the other namespaces, sorted
}

func (e *TriggerTenancyError) Error() string {
	if e.WebhookPath != "" {
		return fmt.Sprintf("%s: webhook path %q is already subscribed from namespace %s; a webhook path may be subscribed from one namespace only",
			ErrTriggerTenancy, e.WebhookPath, quoteNamespaces(e.Others))
	}
	return fmt.Sprintf("%s: trigger %s serves jobs in namespaces %s; a trigger may serve one namespace only (apply one of its jobs to give it its own trigger)",
		ErrTriggerTenancy, e.TriggerID, quoteNamespaces(e.Others))
}

func (e *TriggerTenancyError) Unwrap() error { return ErrTriggerTenancy }

func quoteNamespaces(namespaces []string) string {
	quoted := make([]string, len(namespaces))
	for i, ns := range namespaces {
		quoted[i] = strconv.Quote(ns)
	}
	return strings.Join(quoted, ", ")
}

// TenancyInvariantEnforced reports whether rule 2 applies: a policy file is
// loaded. It reads the process holder at the point of use, so a reload takes
// effect on the next write.
func TenancyInvariantEnforced() bool {
	return policy.ProcessHolder().FileBacked()
}

// servedJob is a live job some trigger serves.
type servedJob struct {
	ID        uuid.UUID
	Alias     string
	Namespace string
	TriggerID uuid.UUID
}

// tenancyTarget is the state one job write proposes.
type tenancyTarget struct {
	JobID     uuid.UUID // uuid.Nil for a new job
	Alias     string
	Namespace string // normalised
	Stored    string // the job's stored namespace, "" for a new job
	TriggerID uuid.UUID
	Path      string // proposed normalized webhook path, "" for none
}

// tenancyVerdict is what a write must do to satisfy the rules.
type tenancyVerdict struct {
	// Detach: write the job onto a fresh trigger row (rule 2 repair).
	Detach bool
}

// evaluateTenancy applies both rules to a target's trigger row, given the
// live jobs the row serves besides the target (peers): a peer outside the
// scope refuses the write; with the invariant enforced, a peer in another
// namespace detaches the target onto a fresh row. The webhook path is then
// checked by evaluatePathTenancy, against subscribers that include the old
// row when the verdict detaches.
func evaluateTenancy(target tenancyTarget, peers []servedJob, scope *TenancyScope, enforce bool) (tenancyVerdict, error) {
	var verdict tenancyVerdict
	for _, peer := range peers {
		if !scope.Allows(peer.Alias, peer.Namespace) {
			return verdict, target.refusal(scope, NamespaceReasonTriggerShared)
		}
	}
	if enforce {
		for _, peer := range peers {
			if models.NamespaceOrDefault(peer.Namespace) != target.Namespace {
				verdict.Detach = true
				break
			}
		}
	}
	return verdict, nil
}

// evaluatePathTenancy applies both rules to the target's webhook path.
func evaluatePathTenancy(target tenancyTarget, subscribers []servedJob, scope *TenancyScope, enforce bool) error {
	if target.Path == "" {
		return nil
	}
	var others []string
	for _, sub := range subscribers {
		if sub.ID == target.JobID && target.JobID != uuid.Nil {
			continue
		}
		if !scope.Allows(sub.Alias, sub.Namespace) {
			return target.refusal(scope, NamespaceReasonWebhookPath)
		}
		if ns := models.NamespaceOrDefault(sub.Namespace); enforce && ns != target.Namespace {
			others = append(others, ns)
		}
	}
	if len(others) == 0 {
		return nil
	}
	slices.Sort(others)
	return &TriggerTenancyError{Alias: target.Alias, WebhookPath: target.Path, Namespace: target.Namespace, Others: slices.Compact(others)}
}

func (t tenancyTarget) refusal(scope *TenancyScope, reason string) *NamespaceAllowlistError {
	var allowed []string
	if scope != nil {
		allowed = slices.Clone(scope.Namespaces)
	}
	return &NamespaceAllowlistError{Alias: t.Alias, RequestedNamespace: t.Namespace, StoredNamespace: t.Stored,
		Allowed: allowed, Reason: reason, WebhookPath: t.Path}
}

// ProposedWebhookPath is the normalized path an HTTP trigger definition
// subscribes, as models.Trigger.ApplyDerivedFields will derive it.
func ProposedWebhookPath(triggerType string, configuration map[string]any) string {
	if models.TriggerType(strings.TrimSpace(triggerType)) != models.TriggerTypeHTTP {
		return ""
	}
	path, _ := configuration["path"].(string)
	return models.NormalizedTriggerPath(path)
}

// servedJobsTx returns the live jobs the triggers serve, in alias lock order,
// locking them when lock is set.
func servedJobsTx(tx *gorm.DB, triggerIDs []uuid.UUID, lock bool) ([]servedJob, error) {
	if len(triggerIDs) == 0 {
		return nil, nil
	}
	query := tx
	if lock {
		query = lockJobRows(tx)
	}
	var rows []models.Job
	if err := query.Model(&models.Job{}).Select("id", "alias", "namespace", "trigger_id").
		Where("trigger_id IN ?", triggerIDs).Order(jobAliasLockOrder(tx)).Find(&rows).Error; err != nil {
		return nil, err
	}
	served := make([]servedJob, 0, len(rows))
	for _, row := range rows {
		served = append(served, servedJob{ID: row.ID, Alias: row.Alias, Namespace: models.NamespaceOrDefault(row.Namespace), TriggerID: row.TriggerID})
	}
	return served, nil
}

// webhookSubscribersTx returns the live jobs served by the live HTTP triggers
// on path, other than the trigger row excluded (uuid.Nil excludes none).
func webhookSubscribersTx(tx *gorm.DB, path string, excluded uuid.UUID, lock bool) ([]servedJob, error) {
	if path == "" {
		return nil, nil
	}
	query := tx.Model(&models.Trigger{}).Where("type = ? AND normalized_path = ?", models.TriggerTypeHTTP, path)
	if excluded != uuid.Nil {
		query = query.Where("id <> ?", excluded)
	}
	var triggerIDs []uuid.UUID
	if err := query.Order("id").Pluck("id", &triggerIDs).Error; err != nil {
		return nil, err
	}
	return servedJobsTx(tx, triggerIDs, lock)
}

// existingTriggerRowTx returns the trigger row id the importer will reuse for
// an existing job (retired rows included, as upsertTriggerTx revives them), or
// uuid.Nil when the job has none.
func existingTriggerRowTx(tx *gorm.DB, job *models.Job) (uuid.UUID, error) {
	if job == nil || job.TriggerID == uuid.Nil {
		return uuid.Nil, nil
	}
	var ids []uuid.UUID
	if err := tx.Unscoped().Model(&models.Trigger{}).Where("id = ?", job.TriggerID).Limit(1).Pluck("id", &ids).Error; err != nil {
		return uuid.Nil, err
	}
	if len(ids) == 0 {
		return uuid.Nil, nil
	}
	return ids[0], nil
}

// checkApplyTenancyTx evaluates a definition's write against both rules,
// reading (and, when lock is set, locking) the stored state in tx. It runs
// before the importer writes anything for the definition.
func checkApplyTenancyTx(tx *gorm.DB, existing *models.Job, def *schema.Definition, scope *TenancyScope, enforce, lock bool) (tenancyVerdict, error) {
	if scope == nil && !enforce {
		return tenancyVerdict{}, nil
	}
	target := tenancyTarget{
		Alias:     strings.TrimSpace(def.Metadata.Alias),
		Namespace: models.NamespaceOrDefault(def.Metadata.Namespace),
		Path:      ProposedWebhookPath(def.Trigger.Type, def.Trigger.Configuration),
	}
	if existing != nil {
		target.JobID = existing.ID
		target.Stored = models.NamespaceOrDefault(existing.Namespace)
		row, err := existingTriggerRowTx(tx, existing)
		if err != nil {
			return tenancyVerdict{}, err
		}
		target.TriggerID = row
	}
	var peers []servedJob
	if target.TriggerID != uuid.Nil {
		served, err := servedJobsTx(tx, []uuid.UUID{target.TriggerID}, lock)
		if err != nil {
			return tenancyVerdict{}, err
		}
		peers = withoutJob(served, target.JobID)
	}
	verdict, err := evaluateTenancy(target, peers, scope, enforce)
	if err != nil {
		return verdict, err
	}
	// After a detach the old row (still serving the peers) is another
	// subscriber of its path; otherwise the row the job keeps is not.
	excluded := target.TriggerID
	if verdict.Detach {
		excluded = uuid.Nil
	}
	subscribers, err := webhookSubscribersTx(tx, target.Path, excluded, lock)
	if err != nil {
		return verdict, err
	}
	return verdict, evaluatePathTenancy(target, subscribers, scope, enforce)
}

func withoutJob(jobs []servedJob, id uuid.UUID) []servedJob {
	if id == uuid.Nil {
		return jobs
	}
	return slices.DeleteFunc(slices.Clone(jobs), func(job servedJob) bool { return job.ID == id })
}

// CheckJobCreateTx is the legacy create's (POST /v1/jobs) tenancy check, run
// in its transaction before it writes: a confined writer may not take an
// alias whose row (retired rows included) lives outside its namespaces, and
// the new HTTP trigger's path must satisfy both rules. The new job gets a new
// trigger row, so only the path rule applies to the trigger.
func CheckJobCreateTx(tx *gorm.DB, alias, namespace, triggerType string, configuration map[string]any, scope *TenancyScope) error {
	enforce := TenancyInvariantEnforced()
	if scope == nil && !enforce {
		return nil
	}
	target := tenancyTarget{
		Alias:     strings.TrimSpace(alias),
		Namespace: models.NamespaceOrDefault(namespace),
		Path:      ProposedWebhookPath(triggerType, configuration),
	}
	if scope != nil && scope.Namespaces != nil {
		var rows []models.Job
		if err := lockJobRows(tx).Unscoped().Model(&models.Job{}).Select("id", "alias", "namespace").
			Where("alias = ?", target.Alias).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			stored := models.NamespaceOrDefault(row.Namespace)
			if !slices.Contains(scope.Namespaces, stored) {
				return &NamespaceAllowlistError{Alias: target.Alias, RequestedNamespace: target.Namespace, StoredNamespace: stored,
					Allowed: slices.Clone(scope.Namespaces), Reason: NamespaceReasonStored}
			}
		}
	}
	subscribers, err := webhookSubscribersTx(tx, target.Path, uuid.Nil, true)
	if err != nil {
		return err
	}
	return evaluatePathTenancy(target, subscribers, scope, enforce)
}

// TriggerPatch is the change a PATCH /v1/triggers/:id proposes.
type TriggerPatch struct {
	TriggerID uuid.UUID
	// Path is the normalized webhook path the configuration proposes when
	// read as an HTTP trigger's; meaningful only when PathChanges is set (the
	// PATCH carries a configuration) and ignored when the locked row is not
	// an HTTP trigger.
	Path        string
	PathChanges bool
}

// CheckTriggerPatchTx authorises a trigger PATCH in its transaction, before
// it writes: it locks the live jobs the trigger serves; a confined writer must
// be inside its scope for every one of them (a job may have moved since the
// middleware decided); with a policy file loaded the row must serve at most
// one namespace; and a changed webhook path must satisfy both rules for the
// row's namespace. An alias-only PATCH runs every check but the path's.
func CheckTriggerPatchTx(tx *gorm.DB, patch TriggerPatch, scope *TenancyScope) error {
	enforce := TenancyInvariantEnforced()
	if scope == nil && !enforce {
		return nil
	}
	served, err := servedJobsTx(tx, []uuid.UUID{patch.TriggerID}, true)
	if err != nil {
		return err
	}
	// Whether the configuration proposes a webhook path depends on the row's
	// type, read here locked (after its jobs, the importer's lock order) so a
	// concurrent apply cannot change it between this check and the write.
	var row models.Trigger
	if err := lockJobRows(tx).Select("id", "type").Where("id = ?", patch.TriggerID).First(&row).Error; err != nil {
		return err
	}
	if row.Type != models.TriggerTypeHTTP {
		patch.Path = ""
	}
	if scope != nil && len(served) == 0 {
		// A trigger serving no job is cluster-only (the middleware's rule);
		// its jobs were retired since the middleware decided.
		return &NamespaceAllowlistError{Reason: NamespaceReasonTriggerShared, WebhookPath: patch.Path}
	}
	namespaces := make([]string, 0, len(served))
	for _, job := range served {
		if !scope.Allows(job.Alias, job.Namespace) {
			return &NamespaceAllowlistError{Reason: NamespaceReasonTriggerShared, WebhookPath: patch.Path}
		}
		namespaces = append(namespaces, job.Namespace)
	}
	slices.Sort(namespaces)
	namespaces = slices.Compact(namespaces)
	if enforce && len(namespaces) > 1 {
		return &TriggerTenancyError{TriggerID: patch.TriggerID, Others: namespaces}
	}
	if !patch.PathChanges || patch.Path == "" || len(namespaces) == 0 {
		// A trigger that serves no job has no namespace to keep apart (and
		// only an unrestricted writer reaches one).
		return nil
	}
	target := tenancyTarget{Namespace: namespaces[0], TriggerID: patch.TriggerID, Path: patch.Path}
	subscribers, err := webhookSubscribersTx(tx, patch.Path, patch.TriggerID, true)
	if err != nil {
		return err
	}
	return evaluatePathTenancy(target, subscribers, scope, enforce)
}

// TriggerServesOutsideTx reports whether the trigger row serves a live job
// outside scope (a nil scope: never). Manifest export uses it, in the read
// snapshot it exports from, to withhold a shared trigger's configuration.
func TriggerServesOutsideTx(tx *gorm.DB, triggerID uuid.UUID, scope *TenancyScope) (bool, error) {
	if scope == nil || triggerID == uuid.Nil {
		return false, nil
	}
	served, err := servedJobsTx(tx, []uuid.UUID{triggerID}, false)
	if err != nil {
		return false, err
	}
	for _, job := range served {
		if !scope.Allows(job.Alias, job.Namespace) {
			return true, nil
		}
	}
	return false, nil
}

// ValidateTriggerTenancy is server lint's read-only preview of both rules
// over a batch of definitions, against conn (a read snapshot for a confined
// caller). It mirrors the order the apply evaluates them in: the definitions
// before the one checked are already written, and the ones after it still
// hold their stored rows. The preview tracks what those earlier writes did to
// the catalog the way the importer does them: a job written onto its existing
// trigger row rewrites that row in place (every job the row serves follows
// its new webhook path, and a retired job revived on it serves it again); a
// new job, or one detached by the rule-2 repair, subscribes on a row of its
// own; and each written job carries its new namespace. Nothing is locked. It
// returns the first violation.
func ValidateTriggerTenancy(ctx context.Context, conn *gorm.DB, defs []schema.Definition, scope *TenancyScope) error {
	enforce := TenancyInvariantEnforced()
	if conn == nil || len(defs) == 0 || (scope == nil && !enforce) {
		return nil
	}
	tx := conn.WithContext(ctx)
	aliases := make([]string, 0, len(defs))
	for i := range defs {
		aliases = append(aliases, strings.TrimSpace(defs[i].Metadata.Alias))
	}
	var existingRows []models.Job
	if err := tx.Unscoped().Model(&models.Job{}).Where("alias IN ?", aliases).Find(&existingRows).Error; err != nil {
		return err
	}
	existingByAlias := make(map[string]*models.Job, len(existingRows))
	for i := range existingRows {
		existingByAlias[strings.TrimSpace(existingRows[i].Alias)] = &existingRows[i]
	}
	var (
		// namespaceOf is the namespace each earlier-written job now has.
		namespaceOf = make(map[string]string, len(defs))
		// ownRow marks earlier-written jobs now on a row of their own (new,
		// or detached from a shared row); proposed and proposedPaths hold
		// their subscriptions.
		ownRow        = make(map[string]bool, len(defs))
		proposed      []servedJob
		proposedPaths []string
		// rowPath is the webhook path of each existing row an earlier
		// definition rewrote in place; revived the retired jobs it revived.
		rowPath = make(map[uuid.UUID]string, len(defs))
		revived = make(map[uuid.UUID][]servedJob)
	)
	// current is the catalog's jobs as the earlier writes left them.
	current := func(jobs []servedJob) []servedJob {
		out := make([]servedJob, 0, len(jobs))
		for _, job := range jobs {
			alias := strings.TrimSpace(job.Alias)
			if ownRow[alias] {
				continue
			}
			if namespace, ok := namespaceOf[alias]; ok {
				job.Namespace = namespace
			}
			out = append(out, job)
		}
		return out
	}
	servedNow := func(row uuid.UUID) ([]servedJob, error) {
		served, err := servedJobsTx(tx, []uuid.UUID{row}, false)
		if err != nil {
			return nil, err
		}
		return append(current(served), revived[row]...), nil
	}
	for i := range defs {
		def := &defs[i]
		existing := existingByAlias[aliases[i]]
		target := tenancyTarget{
			Alias:     aliases[i],
			Namespace: models.NamespaceOrDefault(def.Metadata.Namespace),
			Path:      ProposedWebhookPath(def.Trigger.Type, def.Trigger.Configuration),
		}
		if existing != nil {
			target.JobID = existing.ID
			target.Stored = models.NamespaceOrDefault(existing.Namespace)
			row, err := existingTriggerRowTx(tx, existing)
			if err != nil {
				return err
			}
			target.TriggerID = row
		}
		var peers []servedJob
		if target.TriggerID != uuid.Nil {
			served, err := servedNow(target.TriggerID)
			if err != nil {
				return err
			}
			peers = withoutJob(served, target.JobID)
		}
		verdict, err := evaluateTenancy(target, peers, scope, enforce)
		if err != nil {
			return err
		}
		excluded := target.TriggerID
		if verdict.Detach {
			excluded = uuid.Nil
		}
		subscribers, err := previewSubscribers(tx, target.Path, excluded, rowPath, current, servedNow)
		if err != nil {
			return err
		}
		for j, job := range proposed {
			if proposedPaths[j] == target.Path {
				subscribers = append(subscribers, job)
			}
		}
		if err := evaluatePathTenancy(target, subscribers, scope, enforce); err != nil {
			return err
		}
		namespaceOf[aliases[i]] = target.Namespace
		switch {
		case target.TriggerID == uuid.Nil || verdict.Detach:
			ownRow[aliases[i]] = true
			proposed = append(proposed, servedJob{Alias: aliases[i], Namespace: target.Namespace})
			proposedPaths = append(proposedPaths, target.Path)
		default:
			rowPath[target.TriggerID] = target.Path
			if existing.DeletedAt.Valid {
				revived[target.TriggerID] = append(revived[target.TriggerID], servedJob{
					ID: existing.ID, Alias: aliases[i], Namespace: target.Namespace, TriggerID: target.TriggerID})
			}
		}
	}
	return nil
}

// previewSubscribers is webhookSubscribersTx over the catalog as a lint
// preview's earlier writes left it: rows they rewrote in place subscribe their
// new path (rowPath) instead of the stored one, with the jobs they now serve.
func previewSubscribers(tx *gorm.DB, path string, excluded uuid.UUID, rowPath map[uuid.UUID]string,
	current func([]servedJob) []servedJob, servedNow func(uuid.UUID) ([]servedJob, error)) ([]servedJob, error) {
	if path == "" {
		return nil, nil
	}
	stored, err := webhookSubscribersTx(tx, path, excluded, false)
	if err != nil {
		return nil, err
	}
	subscribers := current(slices.DeleteFunc(stored, func(job servedJob) bool {
		_, rewritten := rowPath[job.TriggerID]
		return rewritten
	}))
	rows := make([]uuid.UUID, 0, len(rowPath))
	for row, rewritten := range rowPath {
		if rewritten == path && row != excluded {
			rows = append(rows, row)
		}
	}
	slices.SortFunc(rows, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	for _, row := range rows {
		served, err := servedNow(row)
		if err != nil {
			return nil, err
		}
		subscribers = append(subscribers, served...)
	}
	return subscribers, nil
}

// WriteRefusal classifies a refused job-definition or trigger write for the
// response. A confined writer (non-nil scope) gets an audited 403 that names
// only what it named itself; an unrestricted writer gets Conflict, the full
// message of a rule-2 conflict (409). Both are zero for any other error.
func WriteRefusal(err error, scope *TenancyScope) (denial *authz.Denial, conflict string) {
	if nsErr, ok := errors.AsType[*NamespaceAllowlistError](err); ok {
		return namespaceDenial(nsErr), ""
	}
	tenancyErr, ok := errors.AsType[*TriggerTenancyError](err)
	if !ok {
		return nil, ""
	}
	if scope == nil {
		return nil, tenancyErr.Error()
	}
	message := "insufficient permissions: the trigger serves jobs in more than one namespace"
	if tenancyErr.WebhookPath != "" {
		message = fmt.Sprintf("insufficient permissions: webhook path %q is subscribed from another namespace", tenancyErr.WebhookPath)
	}
	return tenancyDenial(tenancyErr.Namespace, message), ""
}

func namespaceDenial(e *NamespaceAllowlistError) *authz.Denial {
	switch e.Reason {
	case NamespaceReasonRequested, NamespaceReasonStored:
		namespace := e.RequestedNamespace
		if e.Reason == NamespaceReasonStored {
			namespace = e.StoredNamespace
		}
		return &authz.Denial{Reason: "insufficient_role", Required: models.RoleOperator, Namespaces: []string{namespace},
			Message: fmt.Sprintf("insufficient permissions: %s required in namespace %q", models.RoleOperator, namespace)}
	case NamespaceReasonWebhookPath:
		return tenancyDenial(e.RequestedNamespace, fmt.Sprintf("insufficient permissions: webhook path %q is subscribed by a job outside your access", e.WebhookPath))
	default:
		message := "insufficient permissions"
		if e.Alias != "" {
			message = fmt.Sprintf("insufficient permissions: the trigger of job %q also serves a job outside your access", e.Alias)
		}
		return tenancyDenial(e.RequestedNamespace, message)
	}
}

func tenancyDenial(namespace, message string) *authz.Denial {
	var namespaces []string
	if namespace != "" {
		namespaces = []string{namespace}
	}
	return &authz.Denial{Reason: "trigger_tenancy", Required: models.RoleOperator, Namespaces: namespaces, Message: message}
}

// RedactedTenancyMessage is a lint message for a tenancy violation as a
// confined caller may see it (see WriteRefusal); "" when err is not one.
func RedactedTenancyMessage(err error) string {
	if denial, _ := WriteRefusal(err, &TenancyScope{}); denial != nil {
		return strings.TrimPrefix(denial.Message, "insufficient permissions: ")
	}
	return ""
}

// AliasLockKey and PathLockKey name the PostgreSQL advisory locks job writes
// take (see writeTx).
func AliasLockKey(alias string) string { return "caesium.jobdef.alias:" + strings.TrimSpace(alias) }

func PathLockKey(path string) string { return "caesium.jobdef.webhook-path:" + path }

// definitionLockKeys returns the advisory lock keys a batch of definitions
// takes: every alias and every proposed webhook path, sorted, unique.
func definitionLockKeys(defs ...*schema.Definition) []string {
	keys := make([]string, 0, 2*len(defs))
	for _, def := range defs {
		if def == nil {
			continue
		}
		keys = append(keys, AliasLockKey(def.Metadata.Alias))
		if path := ProposedWebhookPath(def.Trigger.Type, def.Trigger.Configuration); path != "" {
			keys = append(keys, PathLockKey(path))
		}
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

// WriteTx runs fn in one transaction on conn with the importer's contention
// retry, holding the advisory locks for keys on PostgreSQL (see writeTx). The
// legacy create and the trigger PATCH use it for their tenancy-checked writes.
func WriteTx(ctx context.Context, conn *gorm.DB, keys []string, fn func(tx *gorm.DB) error) error {
	slices.Sort(keys)
	keys = slices.Compact(slices.DeleteFunc(keys, func(key string) bool { return key == "" }))
	return withImporterBusyRetry(ctx, func() error { return writeTx(ctx, conn, keys, nil, fn) })
}

// lockSessionHook, when set (tests only), runs on PostgreSQL after a write
// has taken its advisory locks and before its transaction begins.
var lockSessionHook func(keys []string)

// writeTx runs fn in one transaction on conn. On PostgreSQL it first takes a
// session-level advisory lock for each key, in the given (sorted) order, on
// the connection the transaction then runs on, and releases them when the
// transaction has ended. Taking them before BEGIN matters: a transaction-level
// lock taken inside a REPEATABLE READ transaction is acquired after the
// transaction's snapshot, so a writer that waited for it would still read the
// state from before the holder committed (and miss the alias or webhook path
// the holder just created). Every writer takes its advisory locks before any
// row lock, so the two kinds cannot deadlock each other. SQLite and dqlite
// serialise write transactions, so nothing is taken there.
func writeTx(ctx context.Context, conn *gorm.DB, keys []string, opts *sql.TxOptions, fn func(tx *gorm.DB) error) error {
	db := conn.WithContext(ctx)
	if db.Name() != "postgres" || len(keys) == 0 {
		if opts != nil {
			return db.Transaction(fn, opts)
		}
		return db.Transaction(fn)
	}
	return db.Connection(func(pinned *gorm.DB) (err error) {
		defer releaseAdvisoryLocks(ctx, pinned)
		for _, key := range keys {
			if err := pinned.Exec("SELECT pg_advisory_lock(hashtextextended(?, 0))", key).Error; err != nil {
				return err
			}
		}
		if lockSessionHook != nil {
			lockSessionHook(keys)
		}
		if opts != nil {
			return pinned.Transaction(fn, opts)
		}
		return pinned.Transaction(fn)
	})
}

// releaseAdvisoryLocks drops every session-level advisory lock of the pinned
// connection (caesium takes no others). If that fails the connection is
// discarded rather than returned to the pool still holding them.
func releaseAdvisoryLocks(ctx context.Context, pinned *gorm.DB) {
	err := pinned.WithContext(context.WithoutCancel(ctx)).Exec("SELECT pg_advisory_unlock_all()").Error
	if err == nil {
		return
	}
	log.Warn("jobdef: releasing advisory locks failed; discarding the connection", "error", err)
	if conn, ok := pinned.Statement.ConnPool.(*sql.Conn); ok {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
}
