package why

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// why_chain_test.go covers the CLI rendering of the `cache.chain: values`
// predecessor-hash exclusion (infra-deploy A4). Spec §4.3: `caesium why` must
// render the exclusion explicitly "or the skip becomes unexplainable" — a task
// reported `cached` while its predecessor's identity visibly moved reads as a
// cache bug until the exclusion is named.

// chainHitBody is the load-bearing shape: a values-mode CACHE HIT whose blobs
// disagree about predecessorHashes. The server reports the exclusion as a note
// plus a kind:"excluded" entry rather than as a discriminating field.
const chainHitBody = `{
  "runId": "22222222-2222-2222-2222-222222222222",
  "jobId": "11111111-1111-1111-1111-111111111111",
  "taskId": "33333333-3333-3333-3333-333333333333",
  "taskName": "mid",
  "verdict": "CACHE_HIT",
  "status": "cached",
  "cacheEnabled": true,
  "hash": "abc123",
  "summary": "CACHE HIT — every hashed input identical to the cached run; the prior result was reused (task \"mid\" did not execute); predecessor hashes excluded (chain: values)",
  "trigger": {"type": "cron", "alias": "nightly"},
  "baseline": {"kind": "cache_origin", "runId": "55555555-5555-5555-5555-555555555555"},
  "diff": {
    "hashEqual": true,
    "notes": ["predecessor hashes excluded (chain: values)"],
    "changes": [
      {"field": "predecessorHashes", "kind": "excluded", "note": "excluded (chain: values)"}
    ]
  }
}`

func TestRenderTable_ChainExclusionIsRenderedAsANote(t *testing.T) {
	out := renderBody(t, chainHitBody)

	require.Contains(t, out, "note: predecessor hashes excluded (chain: values)",
		"the exclusion must be named on stdout or a values-mode skip is unexplainable")
	require.Contains(t, out, "All hashed inputs are identical",
		"an excluded input is not a discriminating field, so the identical-inputs verdict must stand")
	require.NotContains(t, out, "Discriminating fields",
		"an excluded entry must not be counted as a field that changed")
}

// TestRenderTable_ChainExclusionAlongsideRealChanges: the note is additive — a
// values-mode CACHE MISS still names the field that actually discriminated, and
// the count excludes the marker.
func TestRenderTable_ChainExclusionAlongsideRealChanges(t *testing.T) {
	body := strings.Replace(chainHitBody,
		`"changes": [
      {"field": "predecessorHashes", "kind": "excluded", "note": "excluded (chain: values)"}
    ]`,
		`"changes": [
      {"field": "predecessorHashes", "kind": "excluded", "note": "excluded (chain: values)"},
      {"field": "predecessorOutputs.upstream.token", "kind": "map_entry", "before": "v1", "after": "v2"}
    ]`, 1)
	require.NotEqual(t, chainHitBody, body, "the fixture substitution must have applied")

	out := renderBody(t, body)

	require.Contains(t, out, "note: predecessor hashes excluded (chain: values)")
	require.Contains(t, out, "Discriminating fields (1)",
		"only the real change counts; the exclusion marker must not inflate the count")
	require.Contains(t, out, "predecessorOutputs.upstream.token")
	require.Contains(t, out, "v1")
	require.Contains(t, out, "v2")
}

// TestWhyCommand_ChainExclusionOnStdoutInBothForms is the stream guard from
// CLAUDE.md: machine-readable output goes to stdout only, and the note must be
// present in both the table and --json renderings.
func TestWhyCommand_ChainExclusionOnStdoutInBothForms(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		restoreWhyTestGlobals(t)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(chainHitBody))
		}))

		whyJobID = "11111111-1111-1111-1111-111111111111"
		whyTask = "mid"
		whyPartition = ""
		whyServer = server.URL
		whyAPIKey = ""
		whyJSON = asJSON

		cmd := &cobra.Command{Use: "test"}
		cmd.SetContext(context.Background())
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)

		require.NoError(t, Cmd.RunE(cmd, []string{"22222222-2222-2222-2222-222222222222"}))
		server.Close()

		require.Contains(t, stdout.String(), "predecessor hashes excluded (chain: values)",
			"json=%v: the exclusion must reach stdout", asJSON)
		require.Empty(t, stderr.String(), "json=%v: output must not be split across streams", asJSON)

		if asJSON {
			require.True(t, json.Valid(stdout.Bytes()), "--json stdout was not JSON:\n%s", stdout.String())
		}
	}
}
