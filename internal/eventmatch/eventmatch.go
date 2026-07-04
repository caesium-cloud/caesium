// Package eventmatch provides the shared event-pattern matcher and JSONPath
// extraction used by both the event-trigger router (internal/trigger/event) and
// freshness arrival bindings (internal/freshness).
//
// It lives in a leaf package (depending only on internal/models + stdlib) so
// freshness can reuse the exact matching and JSONPath semantics WITHOUT
// importing internal/trigger/event, which would create an import cycle:
//
//	internal/jobdef -> internal/freshness -> internal/trigger/event ->
//	internal/job -> internal/jobdef/runtime -> internal/jobdef/git -> internal/jobdef
//
// NOTE: internal/trigger/event still keeps its own private copies of these
// helpers (matcher.go + the resolveJSONPath family in event.go). This package
// mirrors them verbatim to avoid the cycle; a follow-up can collapse
// trigger/event onto this leaf (safe — importing a leaf creates no cycle).
// Keep the two in sync until then.
package eventmatch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/caesium-cloud/caesium/internal/models"
)

// EventPattern mirrors internal/trigger/event.EventPattern: a type glob plus an
// optional source and a dotted-path field filter.
type EventPattern struct {
	Type   string            `json:"type"`
	Source string            `json:"source,omitempty"`
	Filter map[string]string `json:"filter,omitempty"`
}

// Matches reports whether the ingested event satisfies the pattern.
func (p EventPattern) Matches(evt *models.IngestedEvent) bool {
	if evt == nil {
		return false
	}
	if !matchesEventType(p.Type, evt.Type) {
		return false
	}
	if strings.TrimSpace(p.Source) != "" && strings.TrimSpace(p.Source) != strings.TrimSpace(evt.Source) {
		return false
	}
	for field, expected := range p.Filter {
		actual, ok := extractField(evt.Data, field)
		if !ok || actual != expected {
			return false
		}
	}
	return true
}

func matchesEventType(pattern, eventType string) bool {
	pattern = strings.TrimSpace(pattern)
	eventType = strings.TrimSpace(eventType)
	if pattern == "" || eventType == "" {
		return false
	}
	if !strings.ContainsAny(pattern, "*?[") {
		return pattern == eventType
	}
	matched, err := path.Match(pattern, eventType)
	return err == nil && matched
}

func extractField(data []byte, fieldPath string) (string, bool) {
	fieldPath = strings.TrimSpace(fieldPath)
	if fieldPath == "" {
		return "", false
	}

	var payload any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return "", false
	}

	current := payload
	for _, segment := range strings.Split(fieldPath, ".") {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			return "", false
		}
		object, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		next, ok := object[segment]
		if !ok {
			return "", false
		}
		current = next
	}

	return stringifyJSONValue(current)
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

// ResolveJSONPathBytes extracts a scalar value from JSON bytes using the event
// trigger JSONPath subset ("$", "$.", dotted, and "[i]"). It is shared by
// arrival bindings so watermark paths follow the same behavior as event param
// mapping.
func ResolveJSONPathBytes(data []byte, jsonPath string) (string, bool) {
	var payload any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return "", false
	}
	return resolveJSONPath(payload, jsonPath)
}

func resolveJSONPath(payload any, jsonPath string) (string, bool) {
	if strings.TrimSpace(jsonPath) == "$" {
		return stringifyJSONValue(payload)
	}

	segments := parseJSONPath(jsonPath)
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

func parseJSONPath(jsonPath string) []string {
	jsonPath = strings.TrimSpace(jsonPath)
	if jsonPath == "" {
		return nil
	}
	switch {
	case strings.HasPrefix(jsonPath, "$."):
		jsonPath = jsonPath[2:]
	case jsonPath == "$":
		return []string{}
	case strings.HasPrefix(jsonPath, "$"):
		jsonPath = strings.TrimPrefix(jsonPath, "$")
		jsonPath = strings.TrimPrefix(jsonPath, ".")
	}
	if jsonPath == "" {
		return nil
	}

	raw := strings.Split(jsonPath, ".")
	segments := make([]string, 0, len(raw))
	for _, segment := range raw {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			return nil
		}
		parsed, ok := parseJSONPathSegment(segment)
		if !ok {
			return nil
		}
		segments = append(segments, parsed...)
	}
	return segments
}

func parseJSONPathSegment(segment string) ([]string, bool) {
	if segment == "" {
		return nil, false
	}
	parts := make([]string, 0, 2)
	for len(segment) > 0 {
		open := strings.IndexByte(segment, '[')
		if open < 0 {
			parts = append(parts, segment)
			break
		}
		if open > 0 {
			parts = append(parts, segment[:open])
		}
		close := strings.IndexByte(segment[open:], ']')
		if close <= 1 {
			return nil, false
		}
		index := segment[open+1 : open+close]
		if _, err := strconv.Atoi(index); err != nil {
			return nil, false
		}
		parts = append(parts, index)
		segment = segment[open+close+1:]
	}
	return parts, len(parts) > 0
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
