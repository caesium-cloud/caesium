package freshness

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// CronSkipDecision is the P1 skip-when-fresh decision for one cron tick.
type CronSkipDecision struct {
	Skip     bool
	Reason   string
	Datasets []string
}

type cronSkipRecord struct {
	decl     models.DatasetDeclaration
	reason   string
	consumed map[string]string
}

// ShouldSkipCronRun decides whether a cron tick can be omitted because every
// produced dataset is fresh and the job's consumed watermarks have not advanced
// since the run that produced the current outputs. It records skipped_fresh rows
// only after the full job-level decision is proven.
func ShouldSkipCronRun(ctx context.Context, db *gorm.DB, jobID uuid.UUID, now time.Time) (CronSkipDecision, error) {
	if db == nil {
		return CronSkipDecision{}, fmt.Errorf("freshness: cron skip requires database connection")
	}
	if jobID == uuid.Nil {
		return CronSkipDecision{}, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	e := &Evaluator{
		db:       db,
		store:    NewStore(db),
		registry: NewRegistry(db),
		now:      func() time.Time { return now },
	}
	return e.shouldSkipCronRun(ctx, jobID)
}

func (e *Evaluator) shouldSkipCronRun(ctx context.Context, jobID uuid.UUID) (CronSkipDecision, error) {
	decls, err := e.registry.ListByJob(ctx, jobID)
	if err != nil {
		return CronSkipDecision{}, err
	}
	graph := newRegistrySnapshot(decls)
	produced := graph.produced
	if len(produced) == 0 {
		return CronSkipDecision{Reason: "job declares no produced datasets"}, nil
	}

	records := make([]cronSkipRecord, 0, len(produced))
	datasets := make([]string, 0, len(produced))
	for _, decl := range produced {
		if !declarationSkipWhenFresh(decl) {
			return CronSkipDecision{Reason: "metadata.datasets.skipWhenFresh is false"}, nil
		}
		if strings.TrimSpace(decl.Freshness) == "" {
			return CronSkipDecision{Reason: fmt.Sprintf("produced dataset %s has no freshness SLO", datasetParamName(decl.Namespace, decl.Name))}, nil
		}
		freshness, err := parsePositiveDuration(decl.Freshness)
		if err != nil {
			return CronSkipDecision{}, fmt.Errorf("freshness cron skip: parse freshness for %s: %w", decl.Name, err)
		}
		maxStaleness, err := parseOptionalDuration(decl.MaxStaleness)
		if err != nil {
			return CronSkipDecision{}, fmt.Errorf("freshness cron skip: parse maxStaleness for %s: %w", decl.Name, err)
		}

		state, exists, err := e.store.Get(ctx, decl.Namespace, decl.Name)
		if err != nil {
			return CronSkipDecision{}, err
		}
		if !exists {
			return CronSkipDecision{Reason: fmt.Sprintf("produced dataset %s has no state", datasetParamName(decl.Namespace, decl.Name))}, nil
		}

		status, reason, _, seen := e.statusFor(decl, state, e.now().UTC(), freshness, maxStaleness, true, "")
		if !seen || status != models.DatasetStatusFresh {
			if reason == "" {
				reason = fmt.Sprintf("produced dataset %s is not fresh", datasetParamName(decl.Namespace, decl.Name))
			}
			return CronSkipDecision{Reason: reason}, nil
		}

		advanced, consumed, advancedReason, err := e.consumedWatermarksBlockSkip(ctx, state, graph.consumesByJob[jobID])
		if err != nil {
			return CronSkipDecision{}, err
		}
		if advanced {
			return CronSkipDecision{Reason: advancedReason}, nil
		}

		records = append(records, cronSkipRecord{
			decl:     decl,
			reason:   "cron tick skipped: " + reason,
			consumed: consumed,
		})
		datasets = append(datasets, datasetParamName(decl.Namespace, decl.Name))
	}

	for _, record := range records {
		if err := e.updateStatus(ctx, record.decl.Namespace, record.decl.Name, models.DatasetStatusFresh, strings.TrimPrefix(record.reason, "cron tick skipped: ")); err != nil {
			return CronSkipDecision{}, err
		}
		if err := e.recordDerivation(ctx, record.decl, models.DatasetDecisionSkippedFresh, record.reason, record.consumed, nil); err != nil {
			return CronSkipDecision{}, err
		}
	}
	sort.Strings(datasets)
	return CronSkipDecision{Skip: true, Reason: "all produced datasets fresh and consumed watermarks unchanged", Datasets: datasets}, nil
}

func declarationSkipWhenFresh(decl models.DatasetDeclaration) bool {
	return decl.SkipWhenFresh == nil || *decl.SkipWhenFresh
}

func (e *Evaluator) consumedWatermarksBlockSkip(ctx context.Context, outputState models.DatasetState, consumes []models.DatasetDeclaration) (bool, map[string]string, string, error) {
	if len(consumes) == 0 {
		return false, map[string]string{}, "", nil
	}

	lastConsumed := decodeConsumedWatermarks(outputState.ConsumedWatermarks)
	current := make(map[string]string, len(consumes))
	advanced := make([]string, 0)
	missingSnapshot := make([]string, 0)
	for _, consume := range consumes {
		name := strings.TrimSpace(consume.Name)
		if name == "" {
			continue
		}
		key := datasetParamName(consume.Namespace, name)
		state, ok, err := e.store.Get(ctx, consume.Namespace, name)
		if err != nil {
			return false, nil, "", err
		}
		watermark := ""
		if ok {
			watermark = state.Watermark
		}
		current[key] = watermark

		previous, hadPrevious := lastConsumed[key]
		if !hadPrevious {
			missingSnapshot = append(missingSnapshot, key)
			continue
		}
		if watermarkAdvancedPast(previous, watermark) {
			advanced = append(advanced, key)
		}
	}
	if len(missingSnapshot) > 0 {
		sort.Strings(missingSnapshot)
		return true, current, "missing consumed watermark snapshot for " + strings.Join(missingSnapshot, ","), nil
	}
	if len(advanced) > 0 {
		sort.Strings(advanced)
		return true, current, "consumed watermark advanced for " + strings.Join(advanced, ","), nil
	}
	return false, current, "", nil
}
