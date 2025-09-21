package diff

import (
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// Diff captures the comparison between desired and existing specs.
type Diff struct {
	Creates []JobSpec
	Updates []Update
	Deletes []JobSpec
}

// Update captures the differences for an existing job.
type Update struct {
	Alias string
	Diff  string
}

// Empty reports whether the diff contains no changes.
func (d Diff) Empty() bool {
	return len(d.Creates) == 0 && len(d.Updates) == 0 && len(d.Deletes) == 0
}

// Compare generates a diff between desired and actual job specs.
func Compare(desired, actual map[string]JobSpec) Diff {
	result := Diff{}
	opts := []cmp.Option{
		cmpopts.EquateEmpty(),
	}

	remaining := make(map[string]JobSpec, len(actual))
	for k, v := range actual {
		remaining[k] = v
	}

	for alias, spec := range desired {
		if _, ok := remaining[alias]; !ok {
			result.Creates = append(result.Creates, spec)
			continue
		}

		if diff := cmp.Diff(remaining[alias], spec, opts...); diff != "" {
			result.Updates = append(result.Updates, Update{Alias: alias, Diff: diff})
		}
		delete(remaining, alias)
	}

	for _, spec := range remaining {
		result.Deletes = append(result.Deletes, spec)
	}

	return result
}
