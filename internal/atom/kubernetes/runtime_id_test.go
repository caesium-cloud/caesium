package kubernetes

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveRuntimeID(t *testing.T) {
	for _, tc := range []struct {
		id, requested, fallback string
		wantNS, wantName        string
		wantErr                 string
	}{
		{id: "caesium-marketing/pod-1", wantNS: "caesium-marketing", wantName: "pod-1"},
		{id: "caesium-marketing/pod-1", requested: "caesium-marketing", wantNS: "caesium-marketing", wantName: "pod-1"},
		{id: "caesium-marketing/pod-1", requested: "default", wantErr: "not the requested"},
		{id: "pod-1", fallback: "caesium", wantNS: "caesium", wantName: "pod-1"},
		{id: "pod-1", requested: "caesium-marketing", fallback: "caesium", wantNS: "caesium-marketing", wantName: "pod-1"},
		{id: "/pod-1", wantErr: "malformed"},
		{id: "ns/", wantErr: "malformed"},
		{id: "a/b/c", wantErr: "malformed"},
	} {
		ns, name, err := resolveRuntimeID(tc.id, tc.requested, tc.fallback)
		if tc.wantErr != "" {
			require.ErrorContains(t, err, tc.wantErr, tc.id)
			continue
		}
		require.NoError(t, err, tc.id)
		require.Equal(t, tc.wantNS, ns, tc.id)
		require.Equal(t, tc.wantName, name, tc.id)
	}
	require.Equal(t, "ns/pod", qualifyRuntimeID("ns", "pod"))
}
