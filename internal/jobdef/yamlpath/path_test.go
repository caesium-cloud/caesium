package yamlpath

import "testing"

func TestIsYAML(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"job.yaml", true}, {"job.YML", true}, {"nested/job.YaMl", true},
		{".yaml", true}, {"job.yaml.json", false}, {"job", false}, {"job.yml/child", false}, {"", false},
	} {
		if got := IsYAML(tc.path); got != tc.want {
			t.Errorf("IsYAML(%q) = %v", tc.path, got)
		}
	}
}
