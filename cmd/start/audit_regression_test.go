package start

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/caesium-cloud/caesium/internal/incident"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestStartRejectsUnexpectedPositionals(t *testing.T) {
	require.NotNil(t, Cmd.Args)
	ran := false
	cmd := &cobra.Command{Use: Cmd.Use, Args: Cmd.Args, SilenceErrors: true, SilenceUsage: true, RunE: func(*cobra.Command, []string) error { ran = true; return nil }}
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"unexpected"})
	require.Error(t, cmd.Execute())
	require.False(t, ran)
	require.Empty(t, stdout.String())
}
func TestIncidentRetryPreservesBothCauses(t *testing.T) {
	for _, cause := range []error{run.ErrJobPaused, run.ErrMaxConcurrentRunsReached} {
		err := incidentRetryError(cause)
		require.ErrorIs(t, err, incident.ErrRetryDeferred)
		require.ErrorIs(t, err, cause)
		require.EqualError(t, err, fmt.Sprintf("%s: %s", incident.ErrRetryDeferred, cause))
	}
	cause := errors.New("other failure")
	require.Same(t, cause, incidentRetryError(cause))
	require.NoError(t, incidentRetryError(nil))
}
