package run

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/baselineevidence"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// hold_decision.go makes the data circuit breaker's admission decision
// answerable by `caesium why` (data-circuit-breaker F5).
//
// The write half snapshots, inside the admission transaction, which holds gated
// a run (or were run past by metadata.onUpstreamHold: run) and the verdicts
// they carried at that instant. The read half renders that snapshot for one
// reader. Nothing here reads a hold row after admission: a repeat breach
// replaces the row's verdicts and a release closes it, and neither may change
// why an old run was skipped.

// Dispositions a run records against the holds it met at admission.
const (
	// HoldDispositionSkipped is a run the gate refused (terminal `skipped`).
	HoldDispositionSkipped = "skipped"
	// HoldDispositionAdmitted is a run that ran on held data because its job
	// declares metadata.onUpstreamHold: run.
	HoldDispositionAdmitted = "admitted"
)

// Detail values on a WhyHold.
const (
	// HoldDetailRecorded: the snapshot was read whole.
	HoldDetailRecorded = "recorded"
	// HoldDetailWithheld: the snapshot names something this reader may not see
	// (the opener's run, or the verdicts of the latest breach).
	HoldDetailWithheld = "withheld"
	// HoldDetailUnrecorded: the run predates the snapshot, so only what its
	// skip reason carries is known.
	HoldDetailUnrecorded = "unrecorded"
)

// runHoldDecision is the persisted form of JobRun.HoldDecision.
type runHoldDecision struct {
	Disposition string       `json:"disposition"`
	Holds       []runHoldRef `json:"holds"`
}

// runHoldRef is one hold as it stood when admission met it.
//
// The two *Namespace fields are the TENANCY namespace of the run that opened
// the hold and of the run whose verdicts the hold carried, read once at
// admission. They are not the dataset namespace, which is a separate, reserved
// axis. A nil one means the run could not be attributed (pruned, or never
// recorded), which a confined reader is never shown past.
type runHoldRef struct {
	HoldID             uuid.UUID        `json:"hold_id"`
	DatasetNamespace   string           `json:"dataset_namespace,omitempty"`
	Dataset            string           `json:"dataset"`
	OpenedAt           time.Time        `json:"opened_at"`
	OccurrenceCount    int              `json:"occurrence_count,omitempty"`
	HeldByRunID        *uuid.UUID       `json:"held_by_run_id,omitempty"`
	HeldByRunNamespace *string          `json:"held_by_run_namespace,omitempty"`
	BreachRunID        *uuid.UUID       `json:"breach_run_id,omitempty"`
	BreachRunNamespace *string          `json:"breach_run_namespace,omitempty"`
	Verdicts           []runHoldVerdict `json:"verdicts,omitempty"`
}

// runHoldVerdict is the part of a DataViolation an explanation names. The
// baseline itself is not kept: BaselineUnowned records that the bound was
// derived from a baseline not computed over the breaching run's own namespace,
// which a confined reader never receives (internal/baselineevidence).
type runHoldVerdict struct {
	Metric          string   `json:"metric"`
	Assertion       string   `json:"assertion"`
	Observed        *float64 `json:"observed,omitempty"`
	Bound           *float64 `json:"bound,omitempty"`
	BaselineUnowned bool     `json:"baseline_unowned,omitempty"`
}

// holdDecisionTx snapshots holds into the JSON stored on the run. It runs in
// the admission transaction, so the verdicts it copies are the ones the gate
// acted on.
func holdDecisionTx(tx *gorm.DB, disposition string, holds []models.DatasetHold) (datatypes.JSON, error) {
	var runIDs []uuid.UUID
	for i := range holds {
		if holds[i].HeldByRunID != nil {
			runIDs = append(runIDs, *holds[i].HeldByRunID)
		}
		if breach := holds[i].LatestBreachRunID(); breach != nil {
			runIDs = append(runIDs, *breach)
		}
	}
	namespaces := make(map[uuid.UUID]string, len(runIDs))
	if len(runIDs) > 0 {
		var rows []struct {
			ID        uuid.UUID
			Namespace string
		}
		if err := tx.Model(&models.JobRun{}).Select("id", "namespace").Where("id IN ?", runIDs).Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, row := range rows {
			namespaces[row.ID] = row.Namespace
		}
	}
	owner := func(id *uuid.UUID) *string {
		if id == nil {
			return nil
		}
		namespace, ok := namespaces[*id]
		if !ok {
			return nil
		}
		return &namespace
	}

	decision := runHoldDecision{Disposition: disposition, Holds: make([]runHoldRef, 0, len(holds))}
	for i := range holds {
		hold := &holds[i]
		ref := runHoldRef{
			HoldID:             hold.ID,
			DatasetNamespace:   hold.Namespace,
			Dataset:            hold.Name,
			OpenedAt:           hold.OpenedAt.UTC(),
			OccurrenceCount:    hold.OccurrenceCount,
			HeldByRunID:        hold.HeldByRunID,
			HeldByRunNamespace: owner(hold.HeldByRunID),
			BreachRunID:        hold.LatestBreachRunID(),
		}
		ref.BreachRunNamespace = owner(ref.BreachRunID)
		var violations []DataViolation
		if len(hold.Violations) > 0 {
			// A row whose verdicts do not decode still has an id and a dataset.
			_ = json.Unmarshal(hold.Violations, &violations)
		}
		for _, v := range violations {
			ref.Verdicts = append(ref.Verdicts, runHoldVerdict{
				Metric:    v.Metric,
				Assertion: v.Assertion,
				Observed:  v.Observed,
				Bound:     v.Bound,
				BaselineUnowned: baselineevidence.Withholds(v.BaselineScope, v.Assertion,
					v.BaselineMedian != nil, v.BaselineSamples != 0),
			})
		}
		decision.Holds = append(decision.Holds, ref)
	}
	encoded, err := json.Marshal(decision)
	if err != nil {
		return nil, fmt.Errorf("run: encode hold decision: %w", err)
	}
	return datatypes.JSON(encoded), nil
}

// WhyHoldDecision is the run-level hold provenance `caesium why` carries on
// WhyTrigger: which holds gated (or were run past by) this run.
type WhyHoldDecision struct {
	// Disposition is HoldDispositionSkipped or HoldDispositionAdmitted.
	Disposition string `json:"disposition"`
	// Holds lists EVERY hold the gate met, oldest first. The first is the one
	// named in the run's skip reason and in the summary line.
	Holds []WhyHold `json:"holds"`
}

// WhyHold is one hold in a WhyHoldDecision, as admission saw it and shaped for
// the reader asking.
type WhyHold struct {
	HoldID uuid.UUID `json:"holdId"`
	// DatasetNamespace is the dataset's own (reserved) namespace, empty in v1,
	// not the tenancy namespace.
	DatasetNamespace string `json:"datasetNamespace"`
	Dataset          string `json:"dataset"`
	// OpenedAt is when the hold opened. Absent on a run recorded before F5.
	OpenedAt *time.Time `json:"openedAt,omitempty"`
	// HeldSinceRunID is the run whose breach opened the hold. Absent when the
	// hold recorded none, when the run predates F5, or when that run's
	// namespace is not one the reader may read.
	HeldSinceRunID *uuid.UUID `json:"heldSinceRunId,omitempty"`
	// Violations are the verdicts the hold carried at admission, absent when
	// the latest breach's namespace is not readable by the reader.
	Violations []WhyHoldViolation `json:"violations,omitempty"`
	// Detail is HoldDetailRecorded, HoldDetailWithheld or HoldDetailUnrecorded.
	Detail string `json:"detail"`
}

// WhyHoldViolation is one assertion verdict of a hold.
type WhyHoldViolation struct {
	Metric    string `json:"metric"`
	Assertion string `json:"assertion"`
	// Observed and Bound are absent for a metric that never arrived. Bound is
	// also absent, with BoundWithheld set, when a confined reader would
	// otherwise receive a bound derived from another owner's baseline.
	Observed      *float64 `json:"observed,omitempty"`
	Bound         *float64 `json:"bound,omitempty"`
	BoundWithheld bool     `json:"boundWithheld,omitempty"`
}

// holdIDInError finds the hold id a hold-skipped task row's error carries
// ("dataset_hold:<ns>/<name> hold=<uuid>").
var holdIDInError = regexp.MustCompile(`hold=([0-9a-fA-F-]{36})`)

// whyHoldDecision shapes a run's recorded hold decision for the reader in ctx.
// sampleError is one of the run's skipped task rows' error text, used only to
// recover the hold id for a run recorded before the snapshot existed.
//
// Tenancy. The dataset's name and the hold id stay: the reader's own job
// declares the dataset under datasets.consumes and the run's task rows already
// carry both. The rest belongs to the producer's tenant. The opener's run id is
// shown only if that run's namespace is readable, the verdicts only if the
// namespace of the run that produced them is, and a delta bound that came from
// a baseline not computed over its own namespace is never shown. A namespace
// that could not be attributed counts as unreadable. This is the rule
// GET /v1/datasets/holds applies to the same hold row (api/rest/service/
// dataset shapeHolds). authz.NamespaceAllowed reads a blank namespace as
// "default", which is right for a run's tenancy namespace and wrong for a
// dataset's, so it is only ever called on the former.
func whyHoldDecision(ctx context.Context, jobRun *models.JobRun, sampleError string) *WhyHoldDecision {
	if len(jobRun.HoldDecision) > 0 {
		var stored runHoldDecision
		if err := json.Unmarshal(jobRun.HoldDecision, &stored); err == nil && len(stored.Holds) > 0 {
			return shapeHoldDecision(ctx, &stored)
		}
	}
	return legacyHoldDecision(jobRun, sampleError)
}

func shapeHoldDecision(ctx context.Context, stored *runHoldDecision) *WhyHoldDecision {
	restricted := authz.Restricted(ctx)
	readable := func(namespace *string) bool {
		if !restricted {
			return true
		}
		return namespace != nil && authz.NamespaceAllowed(ctx, *namespace)
	}

	out := &WhyHoldDecision{Disposition: stored.Disposition, Holds: make([]WhyHold, 0, len(stored.Holds))}
	for i := range stored.Holds {
		ref := &stored.Holds[i]
		openedAt := ref.OpenedAt
		hold := WhyHold{
			HoldID:           ref.HoldID,
			DatasetNamespace: ref.DatasetNamespace,
			Dataset:          ref.Dataset,
			OpenedAt:         &openedAt,
			Detail:           HoldDetailRecorded,
		}
		if ref.HeldByRunID != nil {
			if readable(ref.HeldByRunNamespace) {
				hold.HeldSinceRunID = ref.HeldByRunID
			} else {
				hold.Detail = HoldDetailWithheld
			}
		}
		if readable(ref.BreachRunNamespace) {
			for _, v := range ref.Verdicts {
				violation := WhyHoldViolation{Metric: v.Metric, Assertion: v.Assertion, Observed: v.Observed, Bound: v.Bound}
				if restricted && v.BaselineUnowned {
					violation.Bound, violation.BoundWithheld = nil, true
					hold.Detail = HoldDetailWithheld
				}
				hold.Violations = append(hold.Violations, violation)
			}
		} else {
			hold.Detail = HoldDetailWithheld
		}
		out.Holds = append(out.Holds, hold)
	}
	return out
}

// legacyHoldDecision is what a hold-skipped run recorded before the snapshot
// still tells: the dataset in its skip reason and, from a task row, the hold
// id. Anything else is unknown, and says so.
func legacyHoldDecision(jobRun *models.JobRun, sampleError string) *WhyHoldDecision {
	key, ok := strings.CutPrefix(jobRun.SkipReason, "dataset_hold:")
	if !ok {
		return nil
	}
	namespace, name, _ := strings.Cut(key, "/")
	if name == "" {
		return nil
	}
	hold := WhyHold{DatasetNamespace: namespace, Dataset: name, Detail: HoldDetailUnrecorded}
	if match := holdIDInError.FindStringSubmatch(sampleError); match != nil {
		if id, err := uuid.Parse(match[1]); err == nil {
			hold.HoldID = id
		}
	}
	return &WhyHoldDecision{Disposition: HoldDispositionSkipped, Holds: []WhyHold{hold}}
}

// holdKey is the dataset identity as a skipped run's reason writes it.
func (h WhyHold) holdKey() string {
	return models.DatasetHoldKey(h.DatasetNamespace, h.Dataset)
}

// whyHoldVerdictCap bounds how many assertions one hold lists in the summary.
const whyHoldVerdictCap = 3

// describe renders "dataset <key> held since run <id> (assertion <name>:
// observed <v> vs bound <b>; hold <id>)", dropping each clause the reader may
// not have or the run never recorded.
func (h WhyHold) describe() string {
	out := "dataset " + h.holdKey() + " held"
	if h.HeldSinceRunID != nil {
		out += " since run " + h.HeldSinceRunID.String()
	}

	var parts []string
	switch {
	case len(h.Violations) > 0:
		for i, v := range h.Violations {
			if i == whyHoldVerdictCap {
				parts = append(parts, fmt.Sprintf("+%d more assertion(s)", len(h.Violations)-whyHoldVerdictCap))
				break
			}
			parts = append(parts, "assertion "+v.describe())
		}
	case h.Detail == HoldDetailWithheld:
		parts = append(parts, "assertion detail withheld")
	case h.Detail == HoldDetailUnrecorded:
		parts = append(parts, "no assertion detail was recorded for this run")
	}
	if h.HoldID != uuid.Nil {
		parts = append(parts, "hold "+h.HoldID.String())
	}
	if len(parts) == 0 {
		return out
	}
	return out + " (" + strings.Join(parts, "; ") + ")"
}

// describe renders "<metric>.<assertion>: observed <v> vs bound <b>".
func (v WhyHoldViolation) describe() string {
	name := v.Metric + "." + v.Assertion
	if v.Observed == nil && v.Bound == nil && !v.BoundWithheld {
		return name + ": metric was never emitted"
	}
	observed := "none"
	if v.Observed != nil {
		observed = strconv.FormatFloat(*v.Observed, 'g', -1, 64)
	}
	bound := "none"
	switch {
	case v.BoundWithheld:
		bound = "withheld"
	case v.Bound != nil:
		bound = strconv.FormatFloat(*v.Bound, 'g', -1, 64)
	}
	return fmt.Sprintf("%s: observed %s vs bound %s", name, observed, bound)
}

// whyHoldCap bounds the other held datasets named in a summary; the JSON lists
// them all.
const whyHoldCap = 3

// summarizeHoldSkip is the SKIPPED summary for a run the gate refused. It
// narrates the first hold in full and the others by name.
func summarizeHoldSkip(d *WhyHoldDecision) string {
	out := "SKIPPED — " + d.Holds[0].describe()
	if len(d.Holds) > 1 {
		out += "; also held: " + otherHolds(d.Holds[1:])
	}
	return out
}

// summarizeHoldAdmission is the clause appended to a task's summary when its
// run was admitted onto held data by metadata.onUpstreamHold: run.
func summarizeHoldAdmission(d *WhyHoldDecision) string {
	if d == nil || d.Disposition != HoldDispositionAdmitted || len(d.Holds) == 0 {
		return ""
	}
	out := "admitted by metadata.onUpstreamHold: run on " + d.Holds[0].describe()
	if len(d.Holds) > 1 {
		out += "; also held: " + otherHolds(d.Holds[1:])
	}
	return out
}

func otherHolds(holds []WhyHold) string {
	names := make([]string, 0, whyHoldCap+1)
	for i, h := range holds {
		if i == whyHoldCap {
			names = append(names, fmt.Sprintf("+%d more", len(holds)-whyHoldCap))
			break
		}
		name := h.holdKey()
		if h.HoldID != uuid.Nil {
			name += " (hold " + h.HoldID.String() + ")"
		}
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}
