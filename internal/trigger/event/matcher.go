package event

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/caesium-cloud/caesium/internal/models"
)

type EventPattern struct {
	Type   string            `json:"type"`
	Source string            `json:"source,omitempty"`
	Filter map[string]string `json:"filter,omitempty"`
}

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
