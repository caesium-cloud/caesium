package localrun

import (
	"fmt"
	"maps"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
)

// mergeRunParams layers explicit run parameters over trigger defaults.
// Explicit values win, including an explicit empty string. A nil explicit
// map still receives defaults, matching an empty HTTP or event POST.
func mergeRunParams(def *schema.Definition, explicit map[string]string) (map[string]string, error) {
	defaults, err := triggerDefaultParams(def)
	if err != nil {
		return nil, err
	}
	if len(defaults) == 0 && len(explicit) == 0 {
		return nil, nil
	}
	merged := make(map[string]string, len(defaults)+len(explicit))
	maps.Copy(merged, defaults)
	maps.Copy(merged, explicit)
	return merged, nil
}

// triggerDefaultParams is the set a firing trigger reads after import.
// A non-empty trigger.defaultParams replaces configuration.defaultParams
// (importer upsertTriggerTx). An empty top-level map leaves
// configuration.defaultParams in place, so defaults authored only there
// still apply. Non-string values are stringified with fmt.Sprintf("%v"),
// matching cron extractDefaultParams and freshness triggerDefaultParams.
func triggerDefaultParams(def *schema.Definition) (map[string]string, error) {
	if def == nil {
		return nil, nil
	}
	if len(def.Trigger.DefaultParams) > 0 {
		return maps.Clone(def.Trigger.DefaultParams), nil
	}
	return configurationDefaultParams(def.Trigger.Configuration)
}

func configurationDefaultParams(cfg map[string]any) (map[string]string, error) {
	if len(cfg) == 0 {
		return nil, nil
	}
	raw, ok := cfg["defaultParams"]
	if !ok || raw == nil {
		return nil, nil
	}
	switch values := raw.(type) {
	case map[string]string:
		if len(values) == 0 {
			return nil, nil
		}
		return maps.Clone(values), nil
	case map[string]any:
		if len(values) == 0 {
			return nil, nil
		}
		out := make(map[string]string, len(values))
		for key, value := range values {
			if text, ok := value.(string); ok {
				out[key] = text
				continue
			}
			out[key] = fmt.Sprintf("%v", value)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("trigger.configuration.defaultParams must be a map of string keys and string values")
	}
}
