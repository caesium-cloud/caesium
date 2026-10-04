package atom

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContainerState(t *testing.T) {
	for status, want := range map[string]State{"created": Created, "running": Running, "paused": Invalid, "restarting": Invalid, "removing": Stopping, "exited": Stopped, "dead": Stopped, "": Invalid, "unknown": Invalid, "RUNNING": Invalid} {
		require.Equal(t, want, ContainerState(status), status)
	}
}
func TestResultForExitCode(t *testing.T) {
	for code, want := range map[int]Result{0: Success, 1: Failure, 125: StartupFailure, 126: StartupFailure, 127: StartupFailure, 137: Killed, 143: Terminated, -1: Unknown, 2: Unknown, 999: Unknown} {
		require.Equal(t, want, ResultForExitCode(code), code)
	}
}
