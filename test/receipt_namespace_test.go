//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// receiptManifest reads a run's receipt as token and returns it raw with its
// manifest content hash.
func (s *IntegrationTestSuite) receiptManifest(jobID, runID, token string) (string, string) {
	s.T().Helper()
	body := s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/jobs/"+jobID+"/runs/"+runID+"/receipt", token, "")
	var receipt struct {
		ManifestContentHash string `json:"manifest_content_hash"`
	}
	s.Require().NoError(json.Unmarshal([]byte(body), &receipt), body)
	return body, receipt.ManifestContentHash
}

// TestAuthReceiptManifestHonoursReaderNamespaces (identity-and-access W7-ε):
// a job applied in finance at commit C, moved to marketing with another step
// at D, then re-applied unchanged at C (the newest marketing snapshot is
// reused, so the job's commit C names only finance's snapshot). A receipt
// attests the snapshot at the job's commit, so an unrestricted reader's
// receipt of the marketing run carries finance's topology hash, exactly as
// before; a marketing-confined reader's receipt (GET and verify) carries only
// a snapshot marketing wrote.
func (s *IntegrationTestSuite) TestAuthReceiptManifestHonoursReaderNamespaces() {
	s.requireAuthLane()
	alias := fmt.Sprintf("w7e-receipt-%d", time.Now().UnixNano())
	s.b9Cleanup(alias)
	commitC, commitD := alias+"-c", alias+"-d"
	v := s.b7MintViewers()

	s.b7Apply(s.namespacedJobDefinition(alias, "finance"), commitC)
	job := s.requireJobByAlias(alias)
	s.b7MovedJobBack(alias, commitD)
	s.b7MovedJobBack(alias, commitC)
	s.Equal([]string{commitD, commitC}, s.b7History(job.ID, v.cluster), "re-applying an unchanged topology reuses the newest snapshot")

	historyBody := s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/jobs/"+job.ID+"/topology/history", v.cluster, "")
	var history struct {
		Snapshots []struct {
			GitCommit   string `json:"git_commit"`
			ContentHash string `json:"content_hash"`
		} `json:"snapshots"`
	}
	s.Require().NoError(json.Unmarshal([]byte(historyBody), &history), historyBody)
	hashes := map[string]string{}
	for _, snap := range history.Snapshots {
		hashes[snap.GitCommit] = snap.ContentHash
	}
	financeHash, marketingHash := hashes[commitC], hashes[commitD]
	s.Require().NotEmpty(financeHash, historyBody)
	s.Require().NotEmpty(marketingHash, historyBody)
	s.Require().NotEqual(financeHash, marketingHash, "the move added a step")

	runID := s.triggerRun(job.ID)
	s.Require().Equal("succeeded", s.awaitRun(job.ID, runID, 120*time.Second).Status)

	unrestrictedBody, unrestricted := s.receiptManifest(job.ID, runID, v.cluster)
	s.Contains(unrestrictedBody, `"git_commit":"`+commitC+`"`, "precondition: the job's commit is C again")
	s.Equal(financeHash, unrestricted, "an unrestricted receipt is unchanged: the snapshot at the job's commit")

	body, confined := s.receiptManifest(job.ID, runID, v.mkt)
	s.Equal(marketingHash, confined, "a marketing reader's receipt attests only marketing's snapshot")
	s.NotContains(body, financeHash)

	verifyBody := s.requireNSStatus(http.StatusOK, http.MethodPost, "/v1/jobs/"+job.ID+"/runs/"+runID+"/receipt/verify", v.mkt, body)
	var verified struct {
		Rederived struct {
			ManifestContentHash string `json:"manifest_content_hash"`
		} `json:"rederived"`
	}
	s.Require().NoError(json.Unmarshal([]byte(verifyBody), &verified), verifyBody)
	s.Equal(marketingHash, verified.Rederived.ManifestContentHash, verifyBody)
	s.NotContains(verifyBody, financeHash)
}
