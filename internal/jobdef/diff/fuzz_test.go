package diff

import (
	"os"
	"testing"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
)

func FuzzDecodeDefinitions(f *testing.F) {
	f.Add([]byte("apiVersion: v1\nkind: Job\nmetadata:\n  alias: test\ntrigger:\n  type: cron\n  configuration:\n    cron: \"* * * * *\"\nsteps:\n  - name: step1\n    engine: docker\n    image: alpine\n"))
	f.Add([]byte(""))
	f.Add([]byte("not yaml at all: [[["))
	f.Add([]byte("apiVersion: v1\nkind: Job\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		tmp, err := os.CreateTemp("", "fuzz-def-*.yaml")
		if err != nil {
			t.Skip("cannot create temp file")
		}
		defer os.Remove(tmp.Name())
		if _, err := tmp.Write(data); err != nil {
			tmp.Close()
			t.Skip("cannot write temp file")
		}
		tmp.Close()

		// Must not panic
		_ = decodeDefinitions(tmp.Name(), func(_ *schema.Definition) error { return nil })
	})
}
