package job

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestApplyOutputKeepsCountAndPrintsIDs(t *testing.T) {
	restoreApplyGlobals(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "nightly.job.yaml")
	manifest := []byte(`apiVersion: v1
kind: Job
metadata:
  alias: nightly-etl
trigger:
  type: cron
  configuration:
    cron: "0 2 * * *"
steps:
  - name: extract
    image: alpine:3.23
    command: ["echo", "ok"]
`)
	require.NoError(t, os.WriteFile(path, manifest, 0o644))

	const jobID = "6f1c0c3e-1b1a-4e2a-9c2a-0b1a2c3d4e5f"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1/jobdefs/apply", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"applied":1,"jobs":[{"alias":"nightly-etl","id":"` + jobID + `"}],"contract_warnings":[{"message":"dataset orders is deprecated"}]}`))
	}))
	t.Cleanup(server.Close)

	applyPaths = []string{path}
	applyServer = server.URL
	applyJSON = false

	stdout, stderr, err := execApply(t)
	require.NoError(t, err)
	require.Contains(t, stdout, "Applied 1 job definition(s)")
	require.Contains(t, stdout, "nightly-etl "+jobID)
	require.NotContains(t, stdout, "Warning:")
	require.Contains(t, stderr, "Warning: dataset orders is deprecated")

	applyJSON = true
	stdout, stderr, err = execApply(t)
	require.NoError(t, err)
	require.Contains(t, stderr, "Warning: dataset orders is deprecated")
	require.NotContains(t, stdout, "Warning:")
	require.NotContains(t, stdout, "Applied")
	require.True(t, json.Valid([]byte(stdout)), stdout)
	var doc struct {
		Applied int `json:"applied"`
		Jobs    []struct {
			Alias string `json:"alias"`
			ID    string `json:"id"`
		} `json:"jobs"`
		ContractWarnings []struct {
			Message string `json:"message"`
		} `json:"contract_warnings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &doc))
	require.Equal(t, 1, doc.Applied)
	require.Len(t, doc.Jobs, 1)
	require.Equal(t, "nightly-etl", doc.Jobs[0].Alias)
	require.Equal(t, jobID, doc.Jobs[0].ID)
	require.Equal(t, "dataset orders is deprecated", doc.ContractWarnings[0].Message)
}

func restoreApplyGlobals(t *testing.T) {
	t.Helper()
	origPaths, origServer, origKey := append([]string(nil), applyPaths...), applyServer, applyAPIKey
	origForce, origPrune, origJSON := applyForce, applyPrune, applyJSON
	origBreak, origReason := applyAllowBreaking, applyReason
	origSource, origRepo, origRef := applyProvenanceSourceID, applyProvenanceRepo, applyProvenanceRef
	origCommit, origPath := applyProvenanceCommit, applyProvenancePath
	t.Cleanup(func() {
		applyPaths, applyServer, applyAPIKey = origPaths, origServer, origKey
		applyForce, applyPrune, applyJSON = origForce, origPrune, origJSON
		applyAllowBreaking, applyReason = origBreak, origReason
		applyProvenanceSourceID, applyProvenanceRepo, applyProvenanceRef = origSource, origRepo, origRef
		applyProvenanceCommit, applyProvenancePath = origCommit, origPath
	})
	applyAPIKey = ""
	applyForce = false
	applyPrune = false
	applyAllowBreaking = ""
	applyReason = ""
	applyProvenanceSourceID, applyProvenanceRepo, applyProvenanceRef = "", "", ""
	applyProvenanceCommit, applyProvenancePath = "", ""
	t.Setenv("CAESIUM_API_KEY", "")
}

func execApply(t *testing.T) (string, string, error) {
	t.Helper()
	cmd := &cobra.Command{Use: "apply"}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	err := applyCmd.RunE(cmd, nil)
	return stdout.String(), stderr.String(), err
}
