package job

import (
	"fmt"
	"io"
	"strings"

	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
)

// parseBranchSelection reads container logs for ##caesium::branch markers and
// validates that each selected name is a valid downstream step.  It returns
// the set of selected step names.
//
// If the branch container emits no markers, the returned set is empty (all
// downstream steps should be skipped — short-circuit).
//
// An error is returned if any emitted name is not in validNextSteps.
func parseBranchSelection(logs io.Reader, validNextSteps []string) (map[string]bool, error) {
	branches, err := pkgtask.ParseBranches(logs)
	if err != nil {
		return nil, err
	}

	return validateBranchSelection(branches, validNextSteps)
}

// validateBranchSelection checks that every name in branches is present in
// validNextSteps and returns the set of selected names.  An error is returned
// if any name is invalid.
func validateBranchSelection(branches []string, validNextSteps []string) (map[string]bool, error) {
	validSet := make(map[string]bool, len(validNextSteps))
	for _, name := range validNextSteps {
		validSet[name] = true
	}

	selected := make(map[string]bool, len(branches))
	for _, name := range branches {
		if !validSet[name] {
			return nil, fmt.Errorf("branch selected unknown step %q; valid targets: [%s]",
				name, strings.Join(validNextSteps, ", "))
		}
		selected[name] = true
	}

	return selected, nil
}
