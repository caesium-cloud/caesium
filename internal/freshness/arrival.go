package freshness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	eventmatch "github.com/caesium-cloud/caesium/internal/eventmatch"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ArrivalObserver bridges persisted ingested events into source dataset state.
// It reads declarations on each event so freshly applied bindings are visible
// without a separate reload hook.
type ArrivalObserver struct {
	registry *Registry
	store    *Store
}

// ArrivalAdvance records one source declaration matched and advanced/verified
// by an ingested event. EventID is included for caller logs and future operator
// surfaces; DatasetState does not yet have a durable arrival_event_id field.
type ArrivalAdvance struct {
	EventID   uuid.UUID
	Dataset   string
	Watermark string
	Outcome   Outcome
}

type ArrivalResult struct {
	Advances []ArrivalAdvance
}

var (
	defaultArrivalObserver     *ArrivalObserver
	defaultArrivalObserverOnce sync.Once
)

// NewArrivalObserver constructs an arrival observer over the provided DB.
func NewArrivalObserver(conn *gorm.DB) *ArrivalObserver {
	return &ArrivalObserver{
		registry: NewRegistry(conn),
		store:    NewStore(conn),
	}
}

// DefaultArrivalObserver returns the process-wide observer used by REST
// ingestion controllers.
func DefaultArrivalObserver() *ArrivalObserver {
	defaultArrivalObserverOnce.Do(func() {
		defaultArrivalObserver = NewArrivalObserver(db.Connection())
	})
	return defaultArrivalObserver
}

func (o *ArrivalObserver) Observe(ctx context.Context, evt *models.IngestedEvent) (ArrivalResult, error) {
	if o == nil {
		return ArrivalResult{}, errors.New("freshness: arrival observer is nil")
	}
	if o.registry == nil || o.store == nil {
		return ArrivalResult{}, errors.New("freshness: arrival observer is not configured")
	}
	if evt == nil {
		return ArrivalResult{}, errors.New("freshness: arrival observer requires event")
	}
	if evt.ID == uuid.Nil {
		return ArrivalResult{}, errors.New("freshness: arrival observer requires persisted event id")
	}
	if evt.CreatedAt.IsZero() {
		return ArrivalResult{}, errors.New("freshness: arrival observer requires persisted event time")
	}

	bindings, err := o.arrivalBindings(ctx)
	if err != nil {
		return ArrivalResult{}, err
	}
	if len(bindings) == 0 {
		return ArrivalResult{}, nil
	}

	eventTime := evt.CreatedAt.UTC()
	result := ArrivalResult{Advances: make([]ArrivalAdvance, 0, len(bindings))}
	for _, binding := range bindings {
		if !binding.pattern.Matches(evt) {
			continue
		}

		watermark := ""
		if binding.watermarkPath != "" {
			value, ok := eventmatch.ResolveJSONPathBytes(evt.Data, binding.watermarkPath)
			if !ok {
				log.Warn("freshness: arrival watermark path did not resolve",
					"dataset", binding.name, "event_id", evt.ID, "watermark_path", binding.watermarkPath)
				continue
			}
			watermark = value
		}

		res, err := o.store.Advance(ctx, AdvanceInput{
			Namespace:   binding.namespace,
			Name:        binding.name,
			Watermark:   watermark,
			RunID:       uuid.Nil,
			RunOrder:    eventTime,
			CompletedAt: eventTime,
			Consumed:    nil,
			Backfill:    false,
		})
		if err != nil {
			return ArrivalResult{}, fmt.Errorf("advance arrival dataset %q for event %s: %w", binding.name, evt.ID, err)
		}

		result.Advances = append(result.Advances, ArrivalAdvance{
			EventID:   evt.ID,
			Dataset:   binding.name,
			Watermark: watermark,
			Outcome:   res.Outcome,
		})
		log.Info("freshness: arrival matched dataset",
			"dataset", binding.name, "event_id", evt.ID, "outcome", string(res.Outcome), "watermark", watermark)
		if res.Outcome == OutcomeRegressionDropped || res.Outcome == OutcomeOutOfOrderDropped {
			log.Warn("freshness: arrival watermark write dropped",
				"dataset", binding.name, "event_id", evt.ID, "outcome", string(res.Outcome), "watermark", watermark)
		}
	}

	return result, nil
}

type arrivalBinding struct {
	namespace     *string
	name          string
	pattern       eventmatch.EventPattern
	watermarkPath string
}

func (o *ArrivalObserver) arrivalBindings(ctx context.Context) ([]arrivalBinding, error) {
	decls, err := o.registry.ListArrivalSources(ctx)
	if err != nil {
		return nil, err
	}

	bindings := make([]arrivalBinding, 0, len(decls))
	for i := range decls {
		decl := &decls[i]
		if decl.Direction != models.DatasetDirectionSource || len(decl.ArrivalBinding) == 0 {
			continue
		}

		var arrival schema.Arrival
		if err := json.Unmarshal(decl.ArrivalBinding, &arrival); err != nil {
			return nil, fmt.Errorf("parse arrival binding for dataset %q: %w", decl.Name, err)
		}
		if arrival.Event == nil {
			continue
		}

		name := strings.TrimSpace(decl.Name)
		if name == "" {
			continue
		}
		bindings = append(bindings, arrivalBinding{
			namespace: decl.Namespace,
			name:      name,
			pattern: eventmatch.EventPattern{
				Type:   arrival.Event.Type,
				Filter: arrival.Event.Filter,
			},
			watermarkPath: strings.TrimSpace(arrival.Watermark),
		})
	}
	return bindings, nil
}
