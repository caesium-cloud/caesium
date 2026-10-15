package agentprofile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeProfileFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "profile.yaml")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return path
}

func TestApplyForwardsNamespaceFromYAML(t *testing.T) {
	profile, err := readProfileFile(writeProfileFile(t, "namespace: marketing\nname: mkt-agent\nimage: example/agent:latest\n"))
	require.NoError(t, err)
	require.NotNil(t, profile.Namespace, "a namespace: key must not be silently dropped")
	require.Equal(t, "marketing", *profile.Namespace)

	payload, err := json.Marshal(profile.toRequest())
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(payload, &body))
	require.Equal(t, "marketing", body["namespace"])
	require.Equal(t, "mkt-agent", body["name"])
}

func TestApplyForwardsNamespaceFromJSON(t *testing.T) {
	// A file copied from `agentprofile get --json` echoes its namespace back.
	profile, err := readProfileFile(writeProfileFile(t, `{"id":"x","namespace":"*","name":"shared","image":"img"}`))
	require.NoError(t, err)
	require.NotNil(t, profile.Namespace)
	require.Equal(t, "*", *profile.Namespace)
}

func TestApplyOmitsAbsentNamespace(t *testing.T) {
	profile, err := readProfileFile(writeProfileFile(t, "name: plain\nimage: img\n"))
	require.NoError(t, err)
	require.Nil(t, profile.Namespace)

	payload, err := json.Marshal(profile.toRequest())
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(payload, &body))
	_, present := body["namespace"]
	require.False(t, present, "an absent namespace leaves the server default in charge")
}
