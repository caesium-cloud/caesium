package eventmatch

import "fmt"

func ParseTriggerEventPatterns(cfg map[string]any) ([]EventPattern, error) {
	rawEvents, ok := cfg["events"]
	if !ok || rawEvents == nil {
		return nil, nil
	}
	events, ok := rawEvents.([]any)
	if !ok {
		return nil, fmt.Errorf("trigger.configuration.events must be a list")
	}

	patterns := make([]EventPattern, 0, len(events))
	for idx, rawEvent := range events {
		eventMap, ok := rawEvent.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("trigger.configuration.events[%d] must be an object", idx)
		}
		pattern := EventPattern{Filter: map[string]string{}}
		pattern.Type, _ = eventMap["type"].(string)
		pattern.Source, _ = eventMap["source"].(string)
		if rawFilter, ok := eventMap["filter"]; ok && rawFilter != nil {
			filter, ok := rawFilter.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("trigger.configuration.events[%d].filter must be an object", idx)
			}
			for key, value := range filter {
				if stringValue, ok := value.(string); ok {
					pattern.Filter[key] = stringValue
				}
			}
		}
		patterns = append(patterns, pattern)
	}
	return patterns, nil
}
