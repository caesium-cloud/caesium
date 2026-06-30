package run

import (
	"errors"
	"fmt"
	"strings"

	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
)

const (
	PriorityLowValue    = 1
	PriorityNormalValue = 2
	PriorityHighValue   = 3
)

var ErrInvalidPriority = errors.New("run: invalid priority")

func PriorityValue(priority string) (int, error) {
	switch strings.ToLower(strings.TrimSpace(priority)) {
	case "", jobdefschema.PriorityNormal:
		return PriorityNormalValue, nil
	case jobdefschema.PriorityLow:
		return PriorityLowValue, nil
	case jobdefschema.PriorityHigh:
		return PriorityHighValue, nil
	default:
		return 0, fmt.Errorf("%w %q (must be %q, %q, or %q)",
			ErrInvalidPriority,
			priority,
			jobdefschema.PriorityHigh,
			jobdefschema.PriorityNormal,
			jobdefschema.PriorityLow,
		)
	}
}

func PriorityLabel(priority int) string {
	switch priority {
	case PriorityLowValue:
		return jobdefschema.PriorityLow
	case PriorityHighValue:
		return jobdefschema.PriorityHigh
	default:
		return jobdefschema.PriorityNormal
	}
}
