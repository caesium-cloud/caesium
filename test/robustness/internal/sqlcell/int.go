package sqlcell

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Int64 accepts complete integral cells, refusing ambiguous float64 evidence.
func Int64(value any) (int64, error) {
	switch v := value.(type) {
	case int:
		return int64(v), nil
	case int32:
		return int64(v), nil
	case int64:
		return v, nil
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, fmt.Errorf("invalid integer cell %q: %w", v, err)
		}
		return n, nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid integer cell %q: %w", v, err)
		}
		return n, nil
	case float64:
		// Larger floats may already have rounded before reaching this decoder.
		const maxSafeInteger = 1<<53 - 1
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Trunc(v) != v || math.Abs(v) > maxSafeInteger {
			return 0, fmt.Errorf("unsafe integer cell %v", v)
		}
		return int64(v), nil
	default:
		return 0, fmt.Errorf("unsupported integer cell %T", value)
	}
}

// Bool accepts known boolean strings or integral numeric truth values.
func Bool(value any) (bool, error) {
	switch v := value.(type) {
	case bool:
		return v, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "t":
			return true, nil
		case "0", "false", "f", "":
			return false, nil
		default:
			return false, fmt.Errorf("invalid boolean cell %q", v)
		}
	default:
		n, err := Int64(value)
		if err != nil {
			return false, fmt.Errorf("invalid boolean cell: %w", err)
		}
		return n != 0, nil
	}
}
