package dataset

import (
	"errors"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	runstore "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	"gorm.io/gorm"
)

// State extends the existing freshness row without changing its status semantics.
// Hold fields are omitted when assertions are disabled or no active hold exists.
type State struct {
	models.DatasetState
	HoldStatus string              `json:"hold_status,omitempty"`
	Hold       *models.DatasetHold `json:"hold,omitempty"`
}

// withHolds enriches only the requested page, using exact namespace/name pairs.
func (s *Service) withHolds(rows []models.DatasetState) ([]State, error) {
	states := make([]State, len(rows))
	for i, row := range rows {
		states[i].DatasetState = row
	}
	if !env.Variables().DataAssertionsEnabled || len(rows) == 0 {
		return states, nil
	}
	identity := s.db.Where("namespace = ? AND name = ?", rows[0].Namespace, rows[0].Name)
	for _, row := range rows[1:] {
		identity = identity.Or("namespace = ? AND name = ?", row.Namespace, row.Name)
	}
	var holds []models.DatasetHold
	if err := s.db.WithContext(s.ctx).Where("status = ?", models.DatasetHoldStatusActive).
		Where(identity).Find(&holds).Error; err != nil {
		return nil, err
	}
	byIdentity := make(map[[2]string]*models.DatasetHold, len(holds))
	for i := range holds {
		byIdentity[[2]string{holds[i].Namespace, holds[i].Name}] = &holds[i]
	}
	for i := range states {
		if hold := byIdentity[[2]string{states[i].Namespace, states[i].Name}]; hold != nil {
			states[i].HoldStatus = hold.Status
			states[i].Hold = hold
		}
	}
	return states, nil
}

// ErrHoldStatus identifies an unsupported hold feed filter.
var ErrHoldStatus = errors.New("status must be active, released, or all")

// ErrMetricRequired identifies a missing or reserved metric selector.
var ErrMetricRequired = errors.New("metric is required and must name an emitted metric (not dataset)")

// HoldsParams filters the hold feed. Namespace is a pointer so an explicit empty
// namespace is distinct from an unfiltered feed. Name is an exact match.
type HoldsParams struct {
	Status    string
	Namespace *string
	Name      string
	Limit     int
	Offset    int
}

// HoldsResult is a bounded page of hold history.
type HoldsResult struct {
	Holds  []models.DatasetHold `json:"holds"`
	Total  int64                `json:"total"`
	Limit  int                  `json:"limit"`
	Offset int                  `json:"offset"`
}

// Holds returns active holds by default, or released/all history on request.
func (s *Service) Holds(p HoldsParams) (*HoldsResult, error) {
	status := strings.TrimSpace(p.Status)
	if status == "" {
		status = models.DatasetHoldStatusActive
	}
	if status != models.DatasetHoldStatusActive && status != models.DatasetHoldStatusReleased && status != "all" {
		return nil, ErrHoldStatus
	}
	limit, offset := normalizePagination(p.Limit, p.Offset)
	q := s.db.WithContext(s.ctx).Model(&models.DatasetHold{})
	if status != "all" {
		q = q.Where("status = ?", status)
	}
	if p.Namespace != nil {
		q = q.Where("namespace = ?", strings.TrimSpace(*p.Namespace))
	}
	if name := strings.TrimSpace(p.Name); name != "" {
		q = q.Where("name = ?", name)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, err
	}
	rows := make([]models.DatasetHold, 0)
	if err := q.Order("opened_at DESC").Order("id DESC").Limit(limit).Offset(offset).Find(&rows).Error; err != nil {
		return nil, err
	}
	return &HoldsResult{Holds: rows, Total: total, Limit: limit, Offset: offset}, nil
}

// MetricsResult keeps recent observations separate from the clean baseline:
// rejected samples belong in the series so the breach is visible to operators.
type MetricsResult struct {
	Namespace  string                  `json:"namespace"`
	Name       string                  `json:"name"`
	Metric     string                  `json:"metric"`
	Series     []models.DatasetMetric  `json:"series"`
	Baseline   *runstore.BaselineStats `json:"baseline"`
	Window     int                     `json:"window"`
	MinSamples int                     `json:"min_samples"`
	Seeding    bool                    `json:"seeding"`
}

// Metrics returns recent observations and a separately computed clean baseline.
// A known dataset with no samples for the selected metric has an empty series.
func (s *Service) Metrics(namespace, name, metric string) (*MetricsResult, error) {
	namespace, name, metric = strings.TrimSpace(namespace), strings.TrimSpace(name), strings.TrimSpace(metric)
	if metric == "" || metric == "dataset" {
		return nil, ErrMetricRequired
	}
	exists, err := s.exists(namespace, name)
	if err != nil {
		return nil, err
	}
	if !exists {
		var count int64
		if err := s.db.WithContext(s.ctx).Model(&models.DatasetMetric{}).Where("namespace = ? AND name = ?", namespace, name).Count(&count).Error; err != nil {
			return nil, err
		}
		if count == 0 {
			return nil, gorm.ErrRecordNotFound
		}
	}
	asOf := time.Now().UTC()
	window := runstore.BaselineWindow()
	baseline, err := runstore.Baseline(s.ctx, s.db, namespace, name, metric, window, asOf)
	if err != nil {
		return nil, err
	}
	rows := make([]models.DatasetMetric, 0)
	if err := s.db.WithContext(s.ctx).Where("namespace = ? AND name = ? AND metric = ? AND created_at < ?", namespace, name, metric, asOf).
		Order("created_at DESC").Order("id DESC").Limit(window).Find(&rows).Error; err != nil {
		return nil, err
	}
	for left, right := 0, len(rows)-1; left < right; left, right = left+1, right-1 {
		rows[left], rows[right] = rows[right], rows[left]
	}
	minSamples := runstore.BaselineMinSamples()
	return &MetricsResult{Namespace: namespace, Name: name, Metric: metric, Series: rows, Baseline: baseline,
		Window: window, MinSamples: minSamples, Seeding: baseline.Samples < minSamples}, nil
}
