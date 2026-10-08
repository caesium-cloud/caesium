package jobdef

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/cache"
)

func cacheHashForYAML(t *testing.T, yaml string) string {
	t.Helper()
	def, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	step := &def.Steps[0]
	spec, err := def.RuntimeSpecForStep(step)
	if err != nil {
		t.Fatalf("runtime spec: %v", err)
	}
	return (cache.HashInput{
		JobAlias:             def.Metadata.Alias,
		TaskName:             step.Name,
		Image:                step.Image,
		Command:              step.Command,
		Env:                  spec.Env,
		WorkDir:              spec.WorkDir,
		Mounts:               spec.Mounts,
		ResolvedVolumeMounts: spec.ResolvedVolumeMounts,
		Kubernetes:           spec.Kubernetes,
	}).Compute()
}
