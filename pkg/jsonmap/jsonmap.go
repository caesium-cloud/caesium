package jsonmap

import (
	"fmt"

	"gorm.io/datatypes"
)

// FromStringMap converts a string map into a GORM JSON map value.
func FromStringMap(values map[string]string) datatypes.JSONMap {
	if len(values) == 0 {
		return datatypes.JSONMap{}
	}

	out := datatypes.JSONMap{}
	for key, value := range values {
		out[key] = value
	}
	return out
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
