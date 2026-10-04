// Package bodylimit reads complete HTTP bodies within a configured byte limit.
package bodylimit

import (
	"errors"
	"io"
	"math"
)

var ErrTooLarge = errors.New("request body too large")

// Read retains at most maxBytes bytes when a positive limit is configured.
// A reader failure takes precedence over overflow, so callers can distinguish
// incomplete reads from complete bodies that exceed the limit.
func Read(body io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return io.ReadAll(body)
	}
	readLimit := maxBytes
	if readLimit < math.MaxInt64 {
		readLimit++
	}
	data, err := io.ReadAll(io.LimitReader(body, readLimit))
	tooLarge := int64(len(data)) > maxBytes
	if tooLarge {
		data = data[:maxBytes]
	}
	if err != nil {
		return data, err
	}
	if tooLarge {
		return data, ErrTooLarge
	}
	return data, nil
}
