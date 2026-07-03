package freshness

import (
	"errors"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
)

func produces(alias, name string) models.DatasetDeclaration {
	return models.DatasetDeclaration{JobAlias: alias, Direction: models.DatasetDirectionProduces, Name: name}
}

func consumes(alias, name string) models.DatasetDeclaration {
	return models.DatasetDeclaration{JobAlias: alias, Direction: models.DatasetDirectionConsumes, Name: name}
}

func source(alias, name string, external bool) models.DatasetDeclaration {
	return models.DatasetDeclaration{JobAlias: alias, Direction: models.DatasetDirectionSource, Name: name, External: external}
}

func TestValidateGraphAcceptsValidChain(t *testing.T) {
	decls := []models.DatasetDeclaration{
		source("orders", "raw.vendor_x", true),
		consumes("orders", "raw.vendor_x"),
		produces("orders", "staging.orders"),
		consumes("rollup", "staging.orders"),
		produces("rollup", "analytics.orders_daily"),
	}
	if err := ValidateGraph(decls); err != nil {
		t.Fatalf("expected valid graph, got %v", err)
	}
}

func TestValidateGraphRejectsTwoProducers(t *testing.T) {
	decls := []models.DatasetDeclaration{
		produces("job_a", "analytics.orders_daily"),
		produces("job_b", "analytics.orders_daily"),
	}
	err := ValidateGraph(decls)
	if !errors.Is(err, ErrDatasetMultipleProducers) {
		t.Fatalf("expected ErrDatasetMultipleProducers, got %v", err)
	}
}

func TestValidateGraphRejectsUnresolvedConsume(t *testing.T) {
	decls := []models.DatasetDeclaration{
		consumes("rollup", "nowhere.dataset"),
	}
	err := ValidateGraph(decls)
	if !errors.Is(err, ErrDatasetUnresolvedConsumes) {
		t.Fatalf("expected ErrDatasetUnresolvedConsumes, got %v", err)
	}
}

func TestValidateGraphResolvesExternalSource(t *testing.T) {
	decls := []models.DatasetDeclaration{
		source("orders", "raw.vendor_x", true),
		consumes("orders", "raw.vendor_x"),
	}
	if err := ValidateGraph(decls); err != nil {
		t.Fatalf("external source should resolve consume, got %v", err)
	}
}

func TestValidateGraphRejectsCrossJobCycle(t *testing.T) {
	// job_a produces X and consumes Y; job_b produces Y and consumes X → cycle.
	decls := []models.DatasetDeclaration{
		produces("job_a", "X"),
		consumes("job_a", "Y"),
		produces("job_b", "Y"),
		consumes("job_b", "X"),
	}
	err := ValidateGraph(decls)
	if !errors.Is(err, ErrDatasetGraphCycle) {
		t.Fatalf("expected ErrDatasetGraphCycle, got %v", err)
	}
}

func TestValidateGraphAllowsSelfProduceAndConsume(t *testing.T) {
	// A job consuming a dataset it also produces must not be treated as a cycle
	// (it is a within-job step ordering, not a cross-job derivation loop).
	decls := []models.DatasetDeclaration{
		produces("job_a", "staging.orders"),
		consumes("job_a", "staging.orders"),
	}
	if err := ValidateGraph(decls); err != nil {
		t.Fatalf("self produce/consume must be allowed, got %v", err)
	}
}
