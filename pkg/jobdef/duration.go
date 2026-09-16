package jobdef

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

const durationJSONFormatHint = "must be a duration string (e.g. 1s, 30s) or integer nanoseconds"

// parseJSONDuration accepts documented YAML duration strings ("1s", "30s")
// when a job definition arrives as JSON, and integer nanoseconds, which is
// encoding/json's default time.Duration form. Invalid values name the field
// and the accepted format instead of leaking decoder internals.
func parseJSONDuration(data json.RawMessage, field string) (time.Duration, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0, nil
	}

	var asString string
	if err := json.Unmarshal(trimmed, &asString); err == nil {
		d, err := time.ParseDuration(strings.TrimSpace(asString))
		if err != nil {
			return 0, durationJSONError(field)
		}
		return d, nil
	}

	var asInt int64
	if err := json.Unmarshal(trimmed, &asInt); err == nil {
		return time.Duration(asInt), nil
	}

	var asFloat float64
	if err := json.Unmarshal(trimmed, &asFloat); err == nil && asFloat == math.Trunc(asFloat) {
		return time.Duration(asFloat), nil
	}

	return 0, durationJSONError(field)
}

func durationJSONError(field string) error {
	return fmt.Errorf("%s %s", field, durationJSONFormatHint)
}
