package env

import (
	"math"
	"strconv"
	"testing"
)

func TestParseByteSize(t *testing.T) {
	t.Parallel()

	minKB := int64(math.MinInt64 / 1_000)
	maxKB := int64(math.MaxInt64 / 1_000)
	cases := []struct {
		input string
		want  int64
	}{
		{input: "8", want: 8},
		{input: "8B", want: 8},
		{input: "1KB", want: 1_000},
		{input: "1MB", want: 1_000_000},
		{input: "2MiB", want: 2 << 20},
		{input: strconv.FormatInt(minKB, 10) + "KB", want: minKB * 1_000},
		{input: strconv.FormatInt(maxKB, 10) + "KB", want: maxKB * 1_000},
	}

	for _, tc := range cases {
		got, err := parseByteSize(tc.input)
		if err != nil {
			t.Fatalf("parseByteSize(%q) unexpected error: %v", tc.input, err)
		}
		if got != tc.want {
			t.Fatalf("parseByteSize(%q) = %d, want %d", tc.input, got, tc.want)
		}
	}

	for _, operand := range []int64{minKB - 1, maxKB + 1} {
		input := strconv.FormatInt(operand, 10) + "KB"
		if _, err := parseByteSize(input); err == nil {
			t.Errorf("parseByteSize(%q) unexpectedly accepted an overflowing value", input)
		}
		decoded := ByteSize(-1)
		if err := decoded.Decode(input); err == nil {
			t.Errorf("ByteSize.Decode(%q) unexpectedly accepted an overflowing value", input)
		} else if decoded != -1 {
			t.Errorf("ByteSize.Decode(%q) changed value to %d after error", input, decoded)
		}
	}
}
