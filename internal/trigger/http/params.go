package http

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
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
		return stringifyJSONValue(payload)
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

	return stringifyJSONValue(current)
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

func stringifyJSONValue(value any) (string, bool) {
	switch v := value.(type) {
	case nil:
		return "", false
	case string:
		return v, true
	case json.Number:
		return v.String(), true
	case bool:
		return strconv.FormatBool(v), true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32), true
	case int:
		return strconv.Itoa(v), true
	case int64:
		return strconv.FormatInt(v, 10), true
	case uint64:
		return strconv.FormatUint(v, 10), true
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v), true
		}
		return string(data), true
	}
}
