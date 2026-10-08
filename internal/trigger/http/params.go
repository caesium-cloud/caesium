package http

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/caesium-cloud/caesium/internal/eventmatch"
)

func extractParams(body []byte, mapping map[string]string) map[string]string {
	if len(mapping) == 0 {
		return map[string]string{}
	}

	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return map[string]string{}
	}

	params := make(map[string]string, len(mapping))
	for name, path := range mapping {
		value, ok := resolveJSONPath(payload, path)
		if !ok {
			continue
		}
		params[name] = value
	}
	return params
}

func resolveJSONPath(payload any, path string) (string, bool) {
	if strings.TrimSpace(path) == "$" {
		return eventmatch.StringifyJSONValue(payload)
	}

	segments := parseJSONPath(path)
	if len(segments) == 0 {
		return "", false
	}

	current := payload
	for _, segment := range segments {
		next, ok := descendJSONPath(current, segment)
		if !ok {
			return "", false
		}
		current = next
	}

	return eventmatch.StringifyJSONValue(current)
}

func parseJSONPath(path string) []string {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	switch {
	case strings.HasPrefix(path, "$."):
		path = path[2:]
	case path == "$":
		return []string{}
	case strings.HasPrefix(path, "$"):
		path = strings.TrimPrefix(path, "$")
		path = strings.TrimPrefix(path, ".")
	}
	if path == "" {
		return nil
	}
	raw := strings.Split(path, ".")
	segments := make([]string, 0, len(raw))
	for _, segment := range raw {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			return nil
		}
		segments = append(segments, segment)
	}
	return segments
}

func descendJSONPath(current any, segment string) (any, bool) {
	switch value := current.(type) {
	case map[string]any:
		next, ok := value[segment]
		return next, ok
	case []any:
		index, err := strconv.Atoi(segment)
		if err != nil || index < 0 || index >= len(value) {
			return nil, false
		}
		return value[index], true
	default:
		return nil, false
	}
}
