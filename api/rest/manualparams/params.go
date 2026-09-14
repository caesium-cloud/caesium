// Package manualparams validates parameters accepted by public manual-run
// endpoints. Internal schedulers and routers do not call this package because
// they own the reserved provenance fields.
package manualparams

import (
	"fmt"
	"strings"
)

// Validate rejects scheduler-owned parameter names from a public manual run.
func Validate(params map[string]string) error {
	for key := range params {
		if strings.HasPrefix(key, "_") || key == "logical_date" {
			return fmt.Errorf("run parameter %q is reserved for the scheduler", key)
		}
	}
	return nil
}
