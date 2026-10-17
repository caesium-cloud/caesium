package jobdef

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// The cross-job validators return typed errors so lint can redact what a
// namespace-confined caller may not see (identity-and-access B2); their
// Error() text is what every other caller has always received.
func TestValidatorErrorsKeepTheHistoricalMessages(t *testing.T) {
	cycle := &TriggerChainCycleError{Path: []string{"a", "b", "a"}}
	require.EqualError(t, cycle, "trigger chain cycle detected: a -> b -> a")
	require.ErrorIs(t, cycle, ErrTriggerChainCycle)

	cause := errors.New("bad pattern")
	stored := &TriggerChainJobError{Alias: "fin", Stored: true, Err: cause}
	require.EqualError(t, stored, "existing trigger fin configuration: bad pattern")
	require.ErrorIs(t, stored, cause)
	incoming := &TriggerChainJobError{Alias: "mkt", Err: cause}
	require.EqualError(t, incoming, "definition mkt: bad pattern")

	ref := &ObjectRefNamespaceError{Alias: "job", JobNamespace: "marketing", Field: "metadata.remediation.profile",
		Ref: "triage", ObjectKind: "an AgentProfile", ObjectNamespace: "finance"}
	require.EqualError(t, ref, `job "job" (namespace "marketing"): metadata.remediation.profile "triage" references an AgentProfile in namespace "finance"; a job may reference only objects in its own namespace or the shared namespace "*"`)
	require.Equal(t, `job "job" (namespace "marketing"): metadata.remediation.profile "triage" references an AgentProfile in another namespace; a job may reference only objects in its own namespace or the shared namespace "*"`, ref.Redacted())
}
