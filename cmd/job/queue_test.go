package job

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func renderQueue(t *testing.T, rows []queueItem) string {
	t.Helper()
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	renderQueueTable(cmd, rows)
	return out.String()
}

func TestRenderQueueTableSurfacesClaimState(t *testing.T) {
	enqueued := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	out := renderQueue(t, []queueItem{
		{Position: 1, Priority: 3, EnqueuedAt: enqueued, ClaimState: "stale", Stale: true, ClaimedBy: "node-a/9c1"},
		{Position: 2, Priority: 2, EnqueuedAt: enqueued, ClaimState: "claimed", ClaimedBy: "node-b/44f"},
		{Position: 3, Priority: 1, EnqueuedAt: enqueued, ClaimState: "pending", Params: map[string]string{"lane": "queued"}},
	})

	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.GreaterOrEqual(t, len(lines), 4)
	require.Equal(t, []string{"POSITION", "PRIORITY", "STATE", "REASON", "ENQUEUED_AT", "PARAMS"}, strings.Fields(lines[0]))

	// Every row's state must be a single field, so the table stays parseable.
	require.Equal(t, []string{"1", "high", "stale", "-", "2026-09-09T12:00:00Z", "-"}, strings.Fields(lines[1]))
	require.Equal(t, []string{"2", "normal", "claimed", "-", "2026-09-09T12:00:00Z", "-"}, strings.Fields(lines[2]))
	require.Equal(t, []string{"3", "low", "pending", "-", "2026-09-09T12:00:00Z", "lane=queued"}, strings.Fields(lines[3]))

	require.Contains(t, out, "1 queued run(s) hold an expired claim")
	require.Contains(t, out, "#1 held by node-a/9c1",
		"the footer must name the dead dequeuer so the operator knows which node is stuck")
	require.NotContains(t, out, "node-b/44f",
		"a live claim is not stuck and must not be reported as such")
}

func TestRenderQueueTableOmitsStaleFooterWhenNoneAreStale(t *testing.T) {
	out := renderQueue(t, []queueItem{
		{Position: 1, Priority: 2, EnqueuedAt: time.Now().UTC(), ClaimState: "pending"},
	})
	require.NotContains(t, out, "expired claim")
}

func TestRenderQueueTableTolerantOfServerWithoutClaimState(t *testing.T) {
	out := renderQueue(t, []queueItem{
		{Position: 1, Priority: 2, EnqueuedAt: time.Now().UTC()},
	})
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 2)
	require.Equal(t, "-", strings.Fields(lines[1])[2],
		"an absent claim state must read as unknown, not as pending")
}

// TestRenderQueueTableNamesTheNamespaceQuotaHold pins D2: a row the namespace
// run quota holds says so in its own field and the footer explains it; a row
// without a reason reads "-" and adds no footer.
func TestRenderQueueTableNamesTheNamespaceQuotaHold(t *testing.T) {
	enqueued := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	out := renderQueue(t, []queueItem{
		{Position: 1, Priority: 2, EnqueuedAt: enqueued, ClaimState: "pending", HeldReason: "namespace_quota"},
		{Position: 2, Priority: 2, EnqueuedAt: enqueued, ClaimState: "pending"},
	})
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.GreaterOrEqual(t, len(lines), 3)
	require.Equal(t, []string{"1", "normal", "pending", "namespace_quota", "2026-10-20T12:00:00Z", "-"}, strings.Fields(lines[1]))
	require.Equal(t, []string{"2", "normal", "pending", "-", "2026-10-20T12:00:00Z", "-"}, strings.Fields(lines[2]))
	require.Contains(t, out, "1 queued run(s) wait on their namespace's concurrent-run quota")

	plain := renderQueue(t, []queueItem{{Position: 1, Priority: 2, EnqueuedAt: enqueued, ClaimState: "pending"}})
	require.NotContains(t, plain, "concurrent-run quota")
}

func TestRenderQueueTableEmpty(t *testing.T) {
	require.Equal(t, "No queued runs.\n", renderQueue(t, nil))
}
