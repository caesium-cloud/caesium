package lineage

import (
	"context"
	"testing"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
)

// declaredDefs builds a one-step definition declaring the given produced
// datasets.
func declaredDefs(alias, step string, produces ...string) []schema.Definition {
	ds := &schema.StepDatasets{}
	for _, name := range produces {
		ds.Produces = append(ds.Produces, schema.ProducedDataset{Name: name})
	}
	return []schema.Definition{{
		Metadata: schema.Metadata{Alias: alias},
		Steps:    []schema.Step{{Name: step, Datasets: ds}},
	}}
}

func (s *ImpactSuite) TestCheckDeclaredDatasetsObserved_FlagsUnobservedNames() {
	_, run := s.createJobAndRun("orders-daily", "")
	s.createDataset(run, "caesium", "warehouse/orders", "output", "load")

	warnings, err := CheckDeclaredDatasetsObserved(context.Background(), s.db,
		declaredDefs("orders-daily", "load", "warehouse/orders", "warehouse/ordrs"))
	s.Require().NoError(err)
	s.Require().Len(warnings, 1, "only the never-observed name is reported")
	s.Contains(warnings[0], "warehouse/ordrs")
	s.Contains(warnings[0], "orders-daily/load")
	s.Contains(warnings[0], "typo")
}

func (s *ImpactSuite) TestCheckDeclaredDatasetsObserved_AllObservedIsSilent() {
	_, run := s.createJobAndRun("orders-daily", "")
	s.createDataset(run, "caesium", "warehouse/orders", "output", "load")

	warnings, err := CheckDeclaredDatasetsObserved(context.Background(), s.db,
		declaredDefs("orders-daily", "load", "warehouse/orders"))
	s.Require().NoError(err)
	s.Empty(warnings)
}

func (s *ImpactSuite) TestCheckDeclaredDatasetsObserved_MatchesAcrossNamespaces() {
	// Observed rows carry the configured OpenLineage namespace; declarations
	// key on name alone in v1, so the match must ignore namespace.
	_, run := s.createJobAndRun("orders-daily", "")
	s.createDataset(run, "some-other-namespace", "warehouse/orders", "output", "load")

	warnings, err := CheckDeclaredDatasetsObserved(context.Background(), s.db,
		declaredDefs("orders-daily", "load", "warehouse/orders"))
	s.Require().NoError(err)
	s.Empty(warnings)
}

func (s *ImpactSuite) TestCheckDeclaredDatasetsObserved_NoDeclarationsNoWork() {
	warnings, err := CheckDeclaredDatasetsObserved(context.Background(), s.db, nil)
	s.Require().NoError(err)
	s.Empty(warnings)

	warnings, err = CheckDeclaredDatasetsObserved(context.Background(), nil,
		declaredDefs("a", "b", "c"))
	s.Require().NoError(err)
	s.Empty(warnings)
}

func TestDeclaredDefsHelperCompiles(t *testing.T) {
	if got := declaredDefs("a", "b", "c"); len(got) != 1 {
		t.Fatalf("expected one definition, got %d", len(got))
	}
}
