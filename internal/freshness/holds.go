package freshness

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// heldDataset is an ACTIVE data-circuit-breaker hold on one dataset, reduced to
// what a freshness decision needs. The dataset's identity is the exact
// (namespace, name) pair — never the concatenated "ns/name" key, which is
// ambiguous for a slash-containing name such as "warehouse/orders" (stored
// whole in Name) — so lookups key on datasetIdentity.
type heldDataset struct {
	id        uuid.UUID
	namespace string
	name      string
}

// reason is the status reason a held dataset reports. It leads with the same
// "dataset_hold:<ns>/<name>" token a hold-skipped downstream run records as its
// SkipReason (run.DatasetHoldSkipReason), so one grep follows a dataset from its
// freshness status to the runs it gated, and carries the hold id the way the
// per-task skip text does.
func (h heldDataset) reason() string {
	return fmt.Sprintf("%s hold=%s", runstorage.DatasetHoldSkipReason(h.namespace, h.name), h.id)
}

// activeHolds returns the datasets that currently have an active hold, keyed by
// exact identity. It is one indexed read of a small, low-volume catalog table
// (a hold exists only while a dataset is broken), so a pass loads it ONCE and
// every statusFor/upstreamReady in that pass consults the map instead of
// issuing a query per dataset.
//
// With CAESIUM_DATA_ASSERTIONS_ENABLED off it returns nil WITHOUT querying: the
// breaker is dormant, and a stale `active` row left by a previous flag-on
// period must not change freshness (the admission gate and the API's hold badge
// apply the same gate).
func activeHolds(ctx context.Context, db *gorm.DB) (map[datasetIdentity]heldDataset, error) {
	if db == nil || !runstorage.DataAssertionsEnabled() {
		return nil, nil
	}
	var rows []models.DatasetHold
	if err := db.WithContext(ctx).
		Select("id", "namespace", "name").
		Where("status = ?", models.DatasetHoldStatusActive).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	held := make(map[datasetIdentity]heldDataset, len(rows))
	for _, row := range rows {
		id := datasetIdentity{namespace: row.Namespace, name: strings.TrimSpace(row.Name)}
		// One active hold per dataset is a unique-index invariant; if it were ever
		// violated, the lowest id wins so the reason is at least stable.
		if prior, dup := held[id]; dup && prior.id.String() < row.ID.String() {
			continue
		}
		held[id] = heldDataset{id: row.ID, namespace: row.Namespace, name: id.name}
	}
	return held, nil
}

// holdFor reports the active hold on a declaration's dataset, if any.
func holdFor(holds map[datasetIdentity]heldDataset, decl models.DatasetDeclaration) *heldDataset {
	if len(holds) == 0 {
		return nil
	}
	if h, ok := holds[declarationIdentity(decl)]; ok {
		return &h
	}
	return nil
}

// heldInputs names the held datasets among consumes, sorted, as the
// "waiting on upstream" reason fragment. Empty when none is held.
func heldInputs(holds map[datasetIdentity]heldDataset, consumes []models.DatasetDeclaration) []string {
	if len(holds) == 0 {
		return nil
	}
	var out []string
	seen := map[datasetIdentity]struct{}{}
	for _, consume := range consumes {
		id := declarationIdentity(consume)
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if h, ok := holds[id]; ok {
			out = append(out, fmt.Sprintf("%s (%s)", datasetParamName(consume.Namespace, consume.Name), h.reason()))
		}
	}
	sort.Strings(out)
	return out
}

// HeldReason is the status reason a held dataset reports, for readers outside
// the evaluator (the dataset read API overlays it on a dataset it finds an
// active hold for, so list/detail are honest before the next evaluation).
func HeldReason(namespace, name string, holdID uuid.UUID) string {
	return heldDataset{id: holdID, namespace: namespace, name: name}.reason()
}
