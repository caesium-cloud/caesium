package jobdef

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
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

	var asNumber json.Number
	if err := json.Unmarshal(trimmed, &asNumber); err == nil {
		n, ok := parseExactInt64JSONNumber(string(asNumber))
		if !ok {
			return 0, durationJSONError(field)
		}
		return time.Duration(n), nil
	}

	return 0, durationJSONError(field)
}

// parseExactInt64JSONNumber requires an integral value inside signed 64-bit
// bounds. Plain integers use strconv.ParseInt; exponent and decimal forms are
// parsed exactly so overflow and precision-lossy values cannot convert.
func parseExactInt64JSONNumber(s string) (int64, bool) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, true
	}

	// 4 bits per input rune is above log2(10) per decimal digit, with headroom
	// for the exponent so rounding cannot collapse a non-integer onto an integer.
	// ParseFloat requires the entire string; the unused return is the base.
	prec := uint(len(s)*4 + 32)
	f, _, err := big.ParseFloat(s, 10, prec, big.ToNearestEven)
	if err != nil || !f.IsInt() {
		return 0, false
	}
	i, acc := f.Int(nil)
	if acc != big.Exact || i == nil || !i.IsInt64() {
		return 0, false
	}
	return i.Int64(), true
}

func durationJSONError(field string) error {
	return fmt.Errorf("%s %s", field, durationJSONFormatHint)
}
