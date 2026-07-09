// Package outputdiff compares recorded and reproduced task output maps.
package outputdiff

import (
	"fmt"
	"sort"
	"strings"
)

// Entry is a key/value output present on only one side of a comparison.
type Entry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Change is a key whose recorded and reproduced values differ.
type Change struct {
	Key        string `json:"key"`
	Recorded   string `json:"recorded"`
	Reproduced string `json:"reproduced"`
}

// Diff is the deterministic comparison of recorded vs reproduced outputs.
type Diff struct {
	Added   []Entry  `json:"added,omitempty"`
	Removed []Entry  `json:"removed,omitempty"`
	Changed []Change `json:"changed,omitempty"`
}

// Compare computes the output diff. Inputs are treated as immutable.
func Compare(recorded, reproduced map[string]string) Diff {
	keys := make(map[string]struct{}, len(recorded)+len(reproduced))
	for key := range recorded {
		keys[key] = struct{}{}
	}
	for key := range reproduced {
		keys[key] = struct{}{}
	}

	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)

	var diff Diff
	for _, key := range ordered {
		recordedValue, recordedOK := recorded[key]
		reproducedValue, reproducedOK := reproduced[key]
		switch {
		case !recordedOK && reproducedOK:
			diff.Added = append(diff.Added, Entry{Key: key, Value: reproducedValue})
		case recordedOK && !reproducedOK:
			diff.Removed = append(diff.Removed, Entry{Key: key, Value: recordedValue})
		case recordedOK && reproducedOK && recordedValue != reproducedValue:
			diff.Changed = append(diff.Changed, Change{
				Key:        key,
				Recorded:   recordedValue,
				Reproduced: reproducedValue,
			})
		}
	}
	return diff
}

// Empty reports whether the two output maps matched exactly.
func (d Diff) Empty() bool {
	return len(d.Added) == 0 && len(d.Removed) == 0 && len(d.Changed) == 0
}

// Render returns deterministic human-readable diff text.
func (d Diff) Render() string {
	if d.Empty() {
		return "Output diff vs recorded: match\n"
	}

	var b strings.Builder
	b.WriteString("Output diff vs recorded:\n")
	if len(d.Changed) > 0 {
		b.WriteString("  changed:\n")
		for _, change := range d.Changed {
			_, _ = fmt.Fprintf(&b, "    %s: recorded %q -> reproduced %q\n", change.Key, change.Recorded, change.Reproduced)
		}
	}
	if len(d.Removed) > 0 {
		b.WriteString("  removed:\n")
		for _, entry := range d.Removed {
			_, _ = fmt.Fprintf(&b, "    %s: recorded %q, reproduced <missing>\n", entry.Key, entry.Value)
		}
	}
	if len(d.Added) > 0 {
		b.WriteString("  added:\n")
		for _, entry := range d.Added {
			_, _ = fmt.Fprintf(&b, "    %s: recorded <missing>, reproduced %q\n", entry.Key, entry.Value)
		}
	}
	return b.String()
}
