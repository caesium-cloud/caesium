package sqlcell

import (
	"encoding/json"
	"math"
	"testing"
)

func TestInt64ExactSupportedCells(t *testing.T) {
	cases := []struct {
		value any
		want  int64
	}{
		{int(7), 7}, {int32(-8), -8}, {int64(math.MaxInt64), math.MaxInt64},
		{float64(9), 9}, {float64(-9007199254740991), -9007199254740991},
		{json.Number("9007199254740993"), 9007199254740993},
		{json.Number("9223372036854775807"), math.MaxInt64},
		{json.Number("-9223372036854775808"), math.MinInt64},
		{" +42 \t", 42}, {"-9223372036854775808", math.MinInt64},
	}
	for _, tc := range cases {
		got, err := Int64(tc.value)
		if err != nil || got != tc.want {
			t.Errorf("Int64(%#v) = %d, %v; want %d", tc.value, got, err, tc.want)
		}
	}
}

func TestInt64RejectsMalformedOrAmbiguousEvidence(t *testing.T) {
	for _, value := range []any{
		nil, true, uint64(1), float32(1), []byte("1"), struct{}{},
		math.NaN(), math.Inf(1), math.Inf(-1), float64(1.5),
		float64(1 << 53), float64(-1 << 53), float64(math.MaxInt64),
		json.Number("9223372036854775808"), json.Number("-9223372036854775809"),
		json.Number("1.5"), json.Number("1e3"), "1.5", "1 trailing", "", "no", "9223372036854775808",
	} {
		if got, err := Int64(value); err == nil || got != 0 {
			t.Errorf("Int64(%#v) = %d, %v; want zero and error", value, got, err)
		}
	}
}

func TestBoolSupportedForms(t *testing.T) {
	for _, value := range []any{true, "1", " TRUE ", "t", int(-2), json.Number("3"), float64(4)} {
		if got, err := Bool(value); err != nil || !got {
			t.Errorf("Bool(%#v) = %v, %v; want true", value, got, err)
		}
	}
	for _, value := range []any{false, "0", "FALSE", " f ", "", "  ", int(0), json.Number("0"), float64(0)} {
		if got, err := Bool(value); err != nil || got {
			t.Errorf("Bool(%#v) = %v, %v; want false", value, got, err)
		}
	}
	for _, value := range []any{nil, "2", "yes", "true trailing", float64(0.5), json.Number("1.5")} {
		if got, err := Bool(value); err == nil || got {
			t.Errorf("Bool(%#v) = %v, %v; want false and error", value, got, err)
		}
	}
}
