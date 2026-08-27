package lint

import (
	"fmt"
	"sort"
	"strings"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
)

// writeTarget identifies one disjoint region of a named volume: the volume
// itself plus the subPath within it. Two steps that mount the same volume at
// different subPaths write to different regions on disk and do not conflict,
// so they are tracked separately.
type writeTarget struct {
	volume  string
	subPath string
}

// CheckVolumeWriters returns a warning for every named volume (per
// definition) that is mounted read-write — i.e. without readOnly: true — by
// more than one step at the same subPath. This is the "two read-write mounts
// on one volume" check from spec §8; it is a lint *warning*, not an error
// (spec §11 Open Question 2): a legitimate two-writer case (e.g. two steps
// that each own a disjoint subPath of a shared volume) is real and should
// not be blocked outright, only flagged when the same region is genuinely
// contended.
func CheckVolumeWriters(defs []schema.Definition) []string {
	warnings := make([]string, 0)

	for _, def := range defs {
		writers := make(map[writeTarget][]string)
		var order []writeTarget

		for _, step := range def.Steps {
			for _, mount := range step.VolumeMounts {
				if mount.ReadOnly {
					continue
				}
				volumeName := strings.TrimSpace(mount.Volume)
				if volumeName == "" {
					continue
				}
				key := writeTarget{volume: volumeName, subPath: mount.SubPath}
				if _, ok := writers[key]; !ok {
					order = append(order, key)
				}
				writers[key] = appendUniqueStep(writers[key], step.Name)
			}
		}

		for _, key := range order {
			steps := writers[key]
			if len(steps) < 2 {
				continue
			}
			sorted := append([]string(nil), steps...)
			sort.Strings(sorted)

			var msg string
			if key.subPath != "" {
				msg = fmt.Sprintf("volume %q (subPath %q) is mounted read-write by multiple steps: %s; add readOnly: true to steps that only read",
					key.volume, key.subPath, strings.Join(sorted, ", "))
			} else {
				msg = fmt.Sprintf("volume %q is mounted read-write by multiple steps: %s; add readOnly: true to steps that only read",
					key.volume, strings.Join(sorted, ", "))
			}
			if alias := strings.TrimSpace(def.Metadata.Alias); alias != "" {
				msg = fmt.Sprintf("%s: %s", alias, msg)
			}
			warnings = append(warnings, msg)
		}
	}

	return warnings
}

func appendUniqueStep(steps []string, name string) []string {
	for _, s := range steps {
		if s == name {
			return steps
		}
	}
	return append(steps, name)
}
