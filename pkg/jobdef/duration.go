package jobdef

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
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
// reduced from the decimal coefficient and exponent so overflow, underflow,
// and non-integers cannot convert, and huge exponents cannot allocate a big.Int.
func parseExactInt64JSONNumber(s string) (int64, bool) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, true
	}
	return parseDecimalJSONNumber(s)
}

// maxInt64Digits is the decimal length of math.MaxInt64. A nonzero
// coefficient scaled by 10^exp is outside int64 once the digit count exceeds this.
const maxInt64Digits = 19

// parseDecimalJSONNumber converts a JSON number with a decimal point or
// exponent into an int64. Integrality is decided from the coefficient digits
// and exponent; a nonzero coefficient that would underflow to 0 is rejected.
func parseDecimalJSONNumber(s string) (int64, bool) {
	i := 0
	neg := false
	if i < len(s) && s[i] == '-' {
		neg = true
		i++
	}

	if i >= len(s) || s[i] < '0' || s[i] > '9' {
		return 0, false
	}
	intStart := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	intPart := s[intStart:i]

	fracPart := ""
	if i < len(s) && s[i] == '.' {
		i++
		fracStart := i
		if i >= len(s) || s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		fracPart = s[fracStart:i]
	}

	var exp int64
	expOverflow := false
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		expSign := int64(1)
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			if s[i] == '-' {
				expSign = -1
			}
			i++
		}
		if i >= len(s) || s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		expStart := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		u, err := strconv.ParseInt(s[expStart:i], 10, 64)
		if err != nil {
			expOverflow = true
		} else if expSign < 0 {
			exp = -u
		} else {
			exp = u
		}
	}
	if i != len(s) {
		return 0, false
	}

	if jsonDigitsAllZero(intPart) && jsonDigitsAllZero(fracPart) {
		return 0, true
	}
	if expOverflow {
		return 0, false
	}

	fracLen := int64(len(fracPart))
	if fracLen > 0 && exp < math.MinInt64+fracLen {
		return 0, false
	}
	exp -= fracLen

	digits := intPart + fracPart
	for len(digits) > 0 && digits[0] == '0' {
		digits = digits[1:]
	}
	for len(digits) > 0 && digits[len(digits)-1] == '0' {
		if exp == math.MaxInt64 {
			return 0, false
		}
		digits = digits[:len(digits)-1]
		exp++
	}
	if len(digits) == 0 {
		return 0, true
	}
	if exp < 0 || exp > maxInt64Digits || len(digits)+int(exp) > maxInt64Digits {
		return 0, false
	}

	var buf [1 + maxInt64Digits]byte
	off := 0
	if neg {
		buf[0] = '-'
		off = 1
	}
	off += copy(buf[off:], digits)
	for k := 0; k < int(exp); k++ {
		buf[off] = '0'
		off++
	}
	n, err := strconv.ParseInt(string(buf[:off]), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func jsonDigitsAllZero(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}

func durationJSONError(field string) error {
	return fmt.Errorf("%s %s", field, durationJSONFormatHint)
}
