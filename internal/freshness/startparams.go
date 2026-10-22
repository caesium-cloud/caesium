package freshness

import (
	"context"
	"maps"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ConsumedWatermarksStartParam is the run param holding the watermarks of a
// run's consumed inputs AS THE RUN STARTED — the start-time truth, written by
// EnrichStartParams and read back at completion by Capturer.consumedForRun.
//
// It is deliberately a DIFFERENT key from freshnessConsumedWatermarksParam
// (_consumed_watermarks), which carries a freshness-derived run's
// DERIVATION-time view and belongs to the evaluator alone. The two look alike —
// same JSON shape, same dataset keys — but they answer different questions and
// have different owners:
//
//   - _consumed_watermarks is the view the derivation DECISION was made on. The
//     evaluator stamps it in derive() and matches on it in hasActiveOrQueuedRun
//     to recognise a run it has already scheduled for those inputs. It must stay
//     fixed for the life of the run or that dedupe stops recognising its own run.
//   - _consumed_watermarks_start is the view the run BEGAN on. It is re-taken
//     whenever the run is (re-)created, which for a queued run means at
//     promotion, because that is when it truly starts.
//
// Collapsing them onto one key is what an earlier cut of this did, and it made
// the promotion refresh overwrite the evaluator's decision view: once promotion
// deletes the run_queue row, the running row is all hasActiveOrQueuedRun has
// left to match against, and it no longer matched — so the next tick derived a
// duplicate run for work already in flight.
const ConsumedWatermarksStartParam = "_consumed_watermarks_start"

// enricherNamespace is the namespace the consumed view is read under. v1 always
// keys dataset identity on name alone, matching Capturer.namespace.
var enricherNamespace *string

// EnrichStartParams is the run-store start-params hook that freezes a run's
// consumed-input watermarks onto the run row AT CREATION, under
// ConsumedWatermarksStartParam.
//
// It exists because the consumed view has to be the one the run actually began
// with. Capturing it from an asynchronous run_started subscriber cannot promise
// that: the event only queues work, so the read can land after the run is
// already executing (an input that advanced in between is then credited to a run
// that never read it), and a full subscriber buffer drops the event outright, at
// which point there is no view at all. Enriching the params inside the creation
// path makes the view part of the INSERT: it is there or the run is not.
//
// Every read here goes through the db handle the run store hands in, never a
// captured connection. Runs are created by stores built over an open
// transaction (internal/trigger/event/router.go), and reading a second
// connection underneath one of those deadlocks the whole database.
//
// The result is read back at completion by Capturer.consumedForRun.
//
// On the queued-concurrency path a run is admitted twice: once when it is
// enqueued (the view rides the run_queue row) and again when the dequeuer
// promotes it, which is when the run actually begins. The read is therefore
// unconditional and the second call OVERWRITES the first: a run that waited in
// the queue while its inputs advanced began on the newer view, and keeping the
// admission-time one would attribute its output to inputs it never read — the
// same misattribution this capture exists to prevent, only in the other
// direction (freshness would then see the output as behind and derive redundant
// work).
//
// Because the start-time view has its own key, that refresh costs the evaluator
// nothing: a freshness-derived run's _consumed_watermarks is never read or
// written here, so its decision-time view — and the hasActiveOrQueuedRun dedupe
// that compares it — survives promotion untouched. It is also why fromQueue is
// no longer branched on. When the two views shared a key the flag was what told
// admission's "keep what is already there" apart from promotion's "take it
// again"; with a key of its own there is nothing to keep, and the honest rule is
// simply that every creation of a run re-reads the view that run starts with.
//
// Register it from the server bootstrap under CAESIUM_FRESHNESS_ENABLED. It is a
// plain func rather than a run.StartParamsEnricher so this package keeps no
// dependency on internal/run — the dependency runs the other way.
func EnrichStartParams(ctx context.Context, db *gorm.DB, jobID uuid.UUID, params map[string]string, _ bool) (map[string]string, error) {
	if db == nil || jobID == uuid.Nil {
		return params, nil
	}

	// One indexed read per run creation, which is the whole cost for the common
	// case of a job that declares no dataset. The job's namespace is read in
	// the same statement (listWithOwners): it is the namespace the view below
	// is read through, which FenceStartParams binds to the run's admitted one.
	decls, err := listWithOwners(db.WithContext(ctx).Where("dataset_declarations.job_id = ?", jobID))
	if err != nil {
		return withoutStartView(params), err
	}

	consumedNames, produces := startViewInputs(decls)
	// Nothing produced means the completion handler returns before it ever wants
	// a snapshot; nothing consumed means there is no input view to freeze.
	if !produces || len(consumedNames) == 0 {
		return params, nil
	}

	// A failed read means the view is UNKNOWN, which is not the same answer as
	// an empty one. The param is left ABSENT so completion falls back to its
	// current-watermark read — degraded, but honest. Writing {} here would
	// instead record "this run consumed nothing" as fact, and completion would
	// believe it. It is deliberately not fatal to run creation: freshness is an
	// optional subsystem and a run must still start when its read fails.
	//
	// Absent has to be made true, not just left true: params handed down from an
	// earlier run — the run_queue row on a promotion, the retried run's own row
	// on a retry — still carry that run's view, and persisting it onto a run
	// starting now is exactly the misattribution above. withoutStartView strips
	// it, and run.enrichedStartParams keeps the map returned alongside the error
	// for precisely this case.
	snapshot, err := consumedSnapshot(ctx, db, enricherNamespace, consumedNames)
	if err != nil {
		return withoutStartView(params), err
	}

	// Stamp even an EMPTY view: "no input had a watermark when this run was
	// created" is an authoritative answer, and omitting the param would send the
	// completion path back to the current-watermark read this exists to avoid.
	out := maps.Clone(params)
	if out == nil {
		out = make(map[string]string, 1)
	}
	out[ConsumedWatermarksStartParam] = string(canonicalConsumedJSON(snapshot))
	out[startViewNamespaceParam] = viewNamespace(decls)
	return out, nil
}

// startViewNamespaceParam carries, from EnrichStartParams to FenceStartParams,
// the tenancy namespace the start view was read through. It is transient: the
// fence removes it in the admission transaction, before the run (or its
// run_queue row) is written, so persisted params are exactly what they were
// before identity-and-access B6.
const startViewNamespaceParam = "_consumed_watermarks_start_namespace"

// startViewInputs reports the consumed inputs a start view covers and whether
// the job produces anything (the only case completion records a view for).
func startViewInputs(decls []models.DatasetDeclaration) (consumedNames []string, produces bool) {
	consumedNames = make([]string, 0, len(decls))
	for i := range decls {
		switch decls[i].Direction {
		case models.DatasetDirectionProduces:
			produces = true
		case models.DatasetDirectionConsumes:
			consumedNames = append(consumedNames, decls[i].Name)
		}
	}
	return consumedNames, produces
}

// viewNamespace is the single namespace owning decls as listWithOwners read
// them, or ownerlessSnapshot (never a namespace) when any of them was read
// without a live owner or the owners disagree.
func viewNamespace(decls []models.DatasetDeclaration) string {
	namespace := ""
	for _, decl := range decls {
		owner, snapshotted := snapshotOwner(decl)
		if !snapshotted || owner == nil || (namespace != "" && *owner != namespace) {
			return ownerlessSnapshot
		}
		namespace = *owner
	}
	if namespace == "" {
		return ownerlessSnapshot
	}
	return namespace
}

// FenceStartParams is the run-store admission fence (run.StartParamsFence) for
// the start view EnrichStartParams stamped before the admission transaction.
// namespace is the run's namespace as admission froze it.
//
// A view read through the admitted namespace's declarations is kept exactly as
// stamped. One read through another namespace's (an apply moved the job, and
// possibly changed its inputs, after the enricher's read) would be persisted
// on, and shown with, a run of the new namespace: it is re-taken here, on tx,
// from the declarations the admitted namespace owns now, and removed when that
// cannot be done (they are not that namespace's either, or the read fails),
// which sends completion to its own fenced read. Params carrying no transient
// namespace (no view stamped, or no enricher) come back untouched.
func FenceStartParams(ctx context.Context, tx *gorm.DB, jobID uuid.UUID, namespace string, params map[string]string) (map[string]string, error) {
	captured, ok := params[startViewNamespaceParam]
	if !ok {
		return params, nil
	}
	out := maps.Clone(params)
	delete(out, startViewNamespaceParam)
	if captured == namespace {
		return out, nil
	}
	if tx == nil || jobID == uuid.Nil {
		return withoutStartView(out), nil
	}
	decls, err := listWithOwners(tx.WithContext(ctx).Where("dataset_declarations.job_id = ?", jobID))
	if err != nil {
		return withoutStartView(out), err
	}
	consumedNames, produces := startViewInputs(decls)
	if !produces || len(consumedNames) == 0 || viewNamespace(decls) != namespace {
		return withoutStartView(out), nil
	}
	snapshot, err := consumedSnapshot(ctx, tx, enricherNamespace, consumedNames)
	if err != nil {
		return withoutStartView(out), err
	}
	out[ConsumedWatermarksStartParam] = string(canonicalConsumedJSON(snapshot))
	return out, nil
}

// withoutStartView returns params with any start-time view removed, so a failed
// read can never leave a stale one behind.
//
// It does not ask how the params got one, because more than one path hands them
// down: a promotion carries the admission-time view on the run_queue row, and a
// retry re-runs with the params of the run it is retrying (cmd/run/retry.go and
// api/rest/controller/job/run/retry.go both pass a prior JobRun's Params). Both
// are views of a run that is not this one. Nothing is cloned unless there is
// something to remove.
func withoutStartView(params map[string]string) map[string]string {
	_, view := params[ConsumedWatermarksStartParam]
	_, owner := params[startViewNamespaceParam]
	if !view && !owner {
		return params
	}
	out := maps.Clone(params)
	delete(out, ConsumedWatermarksStartParam)
	delete(out, startViewNamespaceParam)
	return out
}

// namespaceDerivedParams are the run params freshness derives from the
// declarations and dataset state of the namespace a run was created in: the
// start view, its transient owner, and a derived run's decision view and
// triggering dataset.
var namespaceDerivedParams = []string{
	ConsumedWatermarksStartParam,
	startViewNamespaceParam,
	freshnessConsumedWatermarksParam,
	freshnessDerivedFromDatasetParam,
}

// WithoutNamespaceDerivedParams returns params without the keys freshness
// derived from another namespace's datasets, for a run created in a namespace
// other than the one whose params it copies: a replay of a job that moved
// (identity-and-access W7-ε). Those views would otherwise be persisted on, and
// shown with, the new namespace's run and its task descriptors; without a
// start view, completion takes its own fenced read, as after
// FenceStartParams. Nothing is cloned unless there is something to remove.
func WithoutNamespaceDerivedParams(params map[string]string) map[string]string {
	found := false
	for _, key := range namespaceDerivedParams {
		if _, ok := params[key]; ok {
			found = true
			break
		}
	}
	if !found {
		return params
	}
	out := maps.Clone(params)
	for _, key := range namespaceDerivedParams {
		delete(out, key)
	}
	return out
}
