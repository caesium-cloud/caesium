package jsonmap

import (
	"fmt"

	"gorm.io/datatypes"
)

// FromMap converts a map into a GORM JSON map value.
func FromMap[V any](values map[string]V) datatypes.JSONMap {
	if len(values) == 0 {
		return datatypes.JSONMap{}
	}

	out := make(datatypes.JSONMap, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

// FromStringMap converts a string map into a GORM JSON map value.
func FromStringMap(values map[string]string) datatypes.JSONMap {
	return FromMap(values)
}

// Lookup returns the value stored under key if it has type T.
func Lookup[T any](values datatypes.JSONMap, key string) (T, bool) {
	var zero T
	raw, ok := values[key]
	if !ok {
		return zero, false
	}
	typed, ok := raw.(T)
	if !ok {
		return zero, false
	}
	return typed, true
}

// ToStringMap converts a JSON map into a string map.
func ToStringMap(values datatypes.JSONMap) map[string]string {
	if len(values) == 0 {
		return map[string]string{}
	}

	out := make(map[string]string, len(values))
	for key, value := range values {
		if str, ok := value.(string); ok {
			out[key] = str
			continue
		}
		out[key] = fmt.Sprint(value)
	}
	return out
}
