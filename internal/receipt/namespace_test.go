package receipt

import (
	"context"
	"time"

	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
)

// TestManifestHashHonoursHistoryScope (identity-and-access W7-ε): a job moved
// from finance to marketing and re-applied at the commit finance last applied
// (topology unchanged, so the newest marketing row is reused) has a finance
// snapshot at its current commit. A marketing-confined reader's receipt never
// attests it, nor a legacy snapshot that records no namespace; an
// unrestricted receipt is unchanged.
func (s *ReceiptSuite) TestManifestHashHonoursHistoryScope() {
	runID := s.seedRun("moved", "commit-c", "", pinnedSpecs())
	var run models.JobRun
	s.Require().NoError(s.db.Where("id = ?", runID).First(&run).Error)
	s.Require().NoError(s.db.Model(&models.JobRun{}).Where("id = ?", runID).Update("namespace", "marketing").Error)

	base := time.Now().Add(-time.Hour)
	snapshot := func(hash, commit string, namespace *string, at time.Time) {
		s.Require().NoError(s.db.Create(&models.DagSnapshot{
			ID: uuid.New(), JobID: run.JobID, ContentHash: hash, GitCommit: commit, Namespace: namespace,
			Tasks: []byte("[]"), Edges: []byte("[]"), CreatedAt: at,
		}).Error)
	}
	finance, marketing := "finance", "marketing"
	snapshot("legacy-hash", "commit-old", nil, base)
	snapshot("finance-hash", "commit-c", &finance, base.Add(time.Minute))
	snapshot("marketing-hash", "commit-d", &marketing, base.Add(2*time.Minute))

	build := func(ctx context.Context) string {
		r, err := Build(ctx, s.db, runID)
		s.Require().NoError(err)
		return r.ManifestContentHash
	}
	s.Equal("finance-hash", build(context.Background()), "unrestricted: the snapshot at the job's commit, as before")
	marketingOnly := authz.WithAllowedNamespaces(context.Background(), []string{"marketing"})
	s.Equal("marketing-hash", build(marketingOnly), "a marketing reader never attests finance's snapshot")

	// Only a legacy snapshot left: withheld from the confined reader, shown
	// unrestricted.
	s.Require().NoError(s.db.Where("namespace IS NOT NULL").Delete(&models.DagSnapshot{}).Error)
	s.Equal("legacy-hash", build(context.Background()))
	s.Empty(build(marketingOnly))

	// Verify re-derives under the same scope.
	committed, err := Build(marketingOnly, s.db, runID)
	s.Require().NoError(err)
	result, err := Verify(marketingOnly, s.db, committed)
	s.Require().NoError(err)
	s.Empty(result.Rederived.ManifestContentHash)
}
