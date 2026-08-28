package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// stacksFileName is the ordering declaration discover reads from the scan root.
//
// Terraform has no cross-stack dependency declaration — a stack knows nothing
// about the stack that produced a value it consumes — so multi-root mode needs
// somewhere to learn that the VPC stack applies before the app stacks (plan
// Open Question 4). This file is that somewhere.
const stacksFileName = "stacks.yaml"

// stacksFile is the on-disk shape of stacks.yaml.
type stacksFile struct {
	Stacks []stackEntry `yaml:"stacks"`
}

type stackEntry struct {
	Name      string   `yaml:"name"`
	Root      string   `yaml:"root"`
	DependsOn []string `yaml:"dependsOn"`
}

// readStacksFile parses stacks.yaml, returning nil when the file is absent.
//
// Parsing is strict (unknown fields rejected): a typo in a key name would
// otherwise silently drop an ordering constraint, and applying an app stack
// before the network it depends on is the kind of failure that is only visible
// after the fact.
func readStacksFile(path string) ([]stack, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path built from the caller's scan root.
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var parsed stacksFile
	if err := dec.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(parsed.Stacks) == 0 {
		return nil, fmt.Errorf("%s declares no stacks", path)
	}

	stacks := make([]stack, 0, len(parsed.Stacks))
	seen := make(map[string]struct{}, len(parsed.Stacks))
	for i, entry := range parsed.Stacks {
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			return nil, fmt.Errorf("%s: stacks[%d] has no name", path, i)
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("%s: stack %q declared twice", path, name)
		}
		seen[name] = struct{}{}

		root := strings.TrimSpace(entry.Root)
		if root == "" {
			root = name
		}
		if strings.HasPrefix(root, "/") || strings.Contains(root, "..") {
			return nil, fmt.Errorf("%s: stack %q root %q must be a path under the scan root", path, name, root)
		}

		deps := make([]string, 0, len(entry.DependsOn))
		for _, dep := range entry.DependsOn {
			dep = strings.TrimSpace(dep)
			if dep == "" {
				return nil, fmt.Errorf("%s: stack %q has an empty dependsOn entry", path, name)
			}
			deps = append(deps, dep)
		}
		sort.Strings(deps)
		stacks = append(stacks, stack{Name: name, Root: root, DependsOn: deps})
	}
	sort.Slice(stacks, func(i, j int) bool { return stacks[i].Name < stacks[j].Name })
	return stacks, nil
}
