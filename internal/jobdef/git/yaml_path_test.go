package git

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDefaultSourceFilterUsesYAMLExtension(t *testing.T) {
	source := &Source{}
	for _, path := range []string{"jobs/job.YML", "jobs/job.YaMl"} {
		require.True(t, source.shouldInclude(path, path), path)
	}
	for _, path := range []string{"jobs/job.yaml.json", "jobs/job.txt", "jobs/job"} {
		require.False(t, source.shouldInclude(path, path), path)
	}
	source.Globs = []string{"**/*.job.yaml"}
	require.True(t, source.shouldInclude("jobs/job.job.yaml", "jobs/job.job.yaml"))
	require.False(t, source.shouldInclude("jobs/job.YML", "jobs/job.YML"))
}
