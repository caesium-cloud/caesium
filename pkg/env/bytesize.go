package env

import (
	"fmt"
	"strconv"
	"strings"
)

type ByteSize int64

func (b *ByteSize) Decode(value string) error {
	parsed, err := parseByteSize(value)
	if err != nil {
		return err
	}
	*b = ByteSize(parsed)
	return nil
}

func (b ByteSize) Int64() int64 {
	return int64(b)
}

func parseByteSize(value string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, nil
	}

	upper := strings.ToUpper(trimmed)
	type suffixMultiplier struct {
		suffix     string
		multiplier int64
	}
	multipliers := []suffixMultiplier{
		{suffix: "GIB", multiplier: 1 << 30},
		{suffix: "MIB", multiplier: 1 << 20},
		{suffix: "KIB", multiplier: 1 << 10},
		{suffix: "GB", multiplier: 1_000_000_000},
		{suffix: "MB", multiplier: 1_000_000},
		{suffix: "KB", multiplier: 1_000},
		{suffix: "B", multiplier: 1},
	}

	for _, candidate := range multipliers {
		suffix := candidate.suffix
		if !strings.HasSuffix(upper, suffix) {
			continue
		}
		raw := strings.TrimSpace(trimmed[:len(trimmed)-len(suffix)])
		if raw == "" {
			return 0, fmt.Errorf("invalid byte size %q", value)
		}
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid byte size %q: %w", value, err)
		}
		return parsed * candidate.multiplier, nil
	}

	parsed, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid byte size %q: %w", value, err)
	}
	return parsed, nil
}
