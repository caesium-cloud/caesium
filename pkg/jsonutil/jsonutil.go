package jsonutil

import "encoding/json"

// MarshalString marshals the provided value to a JSON string.
func MarshalString[T any](value T) (string, error) {
	buf, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(buf), nil
}

// MarshalMapString marshals a map to a JSON string, substituting an empty map when nil.
func MarshalMapString[K comparable, V any](m map[K]V) (string, error) {
	if m == nil {
		m = map[K]V{}
	}
	return MarshalString(m)
}

// MarshalSliceString marshals a slice to a JSON string, substituting an empty slice when nil.
func MarshalSliceString[T any](values []T) (string, error) {
	if values == nil {
		values = []T{}
	}
	return MarshalString(values)
}
