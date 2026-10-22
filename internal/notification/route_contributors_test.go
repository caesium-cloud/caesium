package notification

import (
	"encoding/json"
	"testing"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/stretchr/testify/require"
)

// withContributors adds the finding_contributors field the contract emitter
// writes when colliding edge ids merged another pair's findings into an edge.
func withContributors(t *testing.T, payload json.RawMessage, contributors any) json.RawMessage {
	t.Helper()
	var fields map[string]any
	require.NoError(t, json.Unmarshal(payload, &fields))
	fields["finding_contributors"] = contributors
	return mustJSON(t, fields)
}

// TestRouteEventWithholdsAFindingMergedFromAForeignEdge (identity-and-access
// W7-ε): marketing's edge producer -> consumer-mkt merged finance's edge
// producer -> consumer-fin, so the finding names two marketing jobs but
// carries finance's requirement. A marketing destination receives nothing of
// it; a shared one receives the event unchanged. A finding whose merged edges
// are all marketing's is delivered without the contributor list, and a
// malformed list delivers nothing.
func TestRouteEventWithholdsAFindingMergedFromAForeignEdge(t *testing.T) {
	f := newRouteFixture(t)
	f.policies(event.TypeContractBreakDeclared, "*", "marketing", "finance")
	f.job("producer", "marketing", nil)
	mkt := f.consumerJob("consumer-mkt", "marketing")
	f.consumerJob("consumer-fin", "finance")
	f.consumerJob("consumer-mkt2", "marketing")
	base := contractPayload(t, "consumer-mkt", [2]string{"producer", "consumer-mkt"})

	foreign := withContributors(t, base, [][]string{{"consumer-fin", "consumer-mkt", "producer"}})
	got := f.route(event.Event{Type: event.TypeContractBreakDeclared, JobID: mkt, Payload: foreign})
	require.Equal(t, []string{"pol-*"}, policyNames(got), "a finding carrying finance's edge never reaches marketing")
	require.Equal(t, foreign, got["pol-*"].Event.Payload)

	own := withContributors(t, base, [][]string{{"consumer-mkt", "consumer-mkt2", "producer"}})
	got = f.route(event.Event{Type: event.TypeContractBreakDeclared, JobID: mkt, Payload: own})
	require.Equal(t, []string{"pol-*", "pol-marketing"}, policyNames(got))
	require.NotContains(t, string(got["pol-marketing"].Event.Payload), "finding_contributors")
	require.Contains(t, string(got["pol-marketing"].Event.Payload), contractKey("consumer-mkt"))

	for name, malformed := range map[string]any{
		"misaligned": [][]string{},
		"not a list": "consumer-mkt",
		"not names":  []any{[]any{1}},
	} {
		got = f.route(event.Event{Type: event.TypeContractBreakDeclared, JobID: mkt, Payload: withContributors(t, base, malformed)})
		require.Equal(t, []string{"pol-*"}, policyNames(got), name)
	}

	// Without the field (no collision) the payload is delivered as before.
	got = f.route(event.Event{Type: event.TypeContractBreakDeclared, JobID: mkt, Payload: base})
	require.Equal(t, base, got["pol-marketing"].Event.Payload)
}
