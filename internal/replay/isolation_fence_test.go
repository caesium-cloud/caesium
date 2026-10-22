package replay

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/stretchr/testify/require"
)

func loadReplayPolicyFile(t *testing.T, body string) *policy.Holder {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	holder, err := policy.LoadFile(context.Background(), policy.HolderOptions{Path: path})
	require.NoError(t, err)
	return holder
}

// TestReplayRowsAreFencedWhenAReloadLandsDuringMaterialization: the replay's
// rows are computed unmarked under `default: {}`, then a reload isolates the
// namespace before they are inserted (its fence sweep could not see them).
// The fence the replay runs after its commit marks them under the current
// revision, so no worker unable to honour the context can pull them. Not
// parallel: it installs the process holder.
func TestReplayRowsAreFencedWhenAReloadLandsDuringMaterialization(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	t.Cleanup(func() { materializeHook = nil })
	f := newReplayFixture(t)
	taskID := f.seedTask(t, seedTaskConfig{name: "safe", replaySafe: true, result: "success"})

	policy.SetProcessHolder(loadReplayPolicyFile(t, "apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  default: {}\n"))
	isolating := loadReplayPolicyFile(t, "apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  default:\n    secrets: {allow: []}\n")
	materializeHook = func() { policy.SetProcessHolder(isolating) }

	result, err := New(f.store, &recordingDispatcher{}).Replay(context.Background(), Request{
		BaselineRunID: f.runID,
		Set:           map[string]string{"mode": "what-if"},
	})
	require.NoError(t, err)
	var row models.TaskRun
	require.NoError(t, f.db.First(&row, "job_run_id = ? AND task_id = ?", result.Run.ID, taskID).Error)
	require.Contains(t, []string{string(run.TaskStatusPending), string(run.TaskStatusRunning)}, row.Status)
	_, want := run.RevisionIsolation(isolating.Current(), models.DefaultNamespace)
	require.Equal(t, want, row.NodeSelector[run.IsolationSelectorKey], "the replay's row is fenced under the revision current at its commit")
}
