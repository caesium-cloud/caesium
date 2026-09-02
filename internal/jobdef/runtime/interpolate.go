package runtime

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// paramRefPattern matches the only scheduler-expanded env form:
// ${CAESIUM_PARAM_<NAME>}. NAME is an identifier; lookup is case-insensitive
// against run-param keys (buildParamEnv uppercases the same way). This is not a
// general expander: bare $CAESIUM_PARAM_*, ${CAESIUM_OUTPUT_*}, ${FOO}, and
// ${CAESIUM_PARAM_SHA:-default} are left unchanged.
var paramRefPattern = regexp.MustCompile(`\$\{CAESIUM_PARAM_([A-Za-z_][A-Za-z0-9_]*)\}`)

// InterpolateParamRefs substitutes ${CAESIUM_PARAM_<NAME>} in env values from
// run parameters. Callers must apply it to the step-declared env before the
// cache hash is computed and before the container is created, so two runs with
// different params cannot cache-hit on a shared token and so reagents that are
// not a shell (git-source GIT_REF) see the substituted value.
//
// Unresolved references fail closed: the token is never left in place and never
// replaced with an empty string. An explicitly empty param value is present and
// is substituted as empty. Param keys that fold to the same strings.ToUpper
// name (SHA vs sha) fail closed so the selected value cannot depend on map
// iteration order. After substitution, a value that is a secret:// URI and
// differs from the job-definition original is rejected: secret identifiers
// must stay static so a trigger caller cannot select which secret is resolved.
// The returned map is a copy; env is never mutated. Command/args are not
// interpolated.
func InterpolateParamRefs(env map[string]string, params map[string]string) (map[string]string, error) {
	lookup, err := paramLookup(params)
	if err != nil {
		return nil, err
	}
	if len(env) == 0 {
		return env, nil
	}

	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make(map[string]string, len(env))
	var missing []string
	seenMissing := make(map[string]struct{})
	var secretChanged []string
	for _, key := range keys {
		original := env[key]
		interpolated, unresolved := interpolateParamRefsInValue(original, lookup)
		out[key] = interpolated
		for _, name := range unresolved {
			report := fmt.Sprintf("env %s: unresolved ${CAESIUM_PARAM_%s}", key, name)
			if _, ok := seenMissing[report]; ok {
				continue
			}
			seenMissing[report] = struct{}{}
			missing = append(missing, report)
		}
		if len(unresolved) == 0 && interpolated != original && strings.HasPrefix(interpolated, "secret://") {
			secretChanged = append(secretChanged, fmt.Sprintf("env %s: interpolation must not produce or modify a secret:// URI", key))
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("interpolate run params: %s", strings.Join(missing, "; "))
	}
	if len(secretChanged) > 0 {
		return nil, fmt.Errorf("interpolate run params: %s", strings.Join(secretChanged, "; "))
	}
	return out, nil
}

func paramLookup(params map[string]string) (map[string]string, error) {
	if len(params) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	lookup := make(map[string]string, len(params))
	folded := make(map[string][]string, len(params))
	for _, k := range keys {
		upper := strings.ToUpper(k)
		folded[upper] = append(folded[upper], k)
		lookup[upper] = params[k]
	}

	var collisions []string
	uppers := make([]string, 0, len(folded))
	for upper := range folded {
		uppers = append(uppers, upper)
	}
	sort.Strings(uppers)
	for _, upper := range uppers {
		group := folded[upper]
		if len(group) < 2 {
			continue
		}
		collisions = append(collisions, fmt.Sprintf("colliding param keys %s", strings.Join(group, " and ")))
	}
	if len(collisions) > 0 {
		return nil, fmt.Errorf("interpolate run params: %s", strings.Join(collisions, "; "))
	}
	return lookup, nil
}

func interpolateParamRefsInValue(value string, lookup map[string]string) (string, []string) {
	if !strings.Contains(value, "${CAESIUM_PARAM_") {
		return value, nil
	}

	var unresolved []string
	replaced := paramRefPattern.ReplaceAllStringFunc(value, func(match string) string {
		sub := paramRefPattern.FindStringSubmatch(match)
		if len(sub) != 2 {
			return match
		}
		name := strings.ToUpper(sub[1])
		v, ok := lookup[name]
		if !ok {
			unresolved = append(unresolved, name)
			return match
		}
		return v
	})
	if len(unresolved) > 0 {
		return value, unresolved
	}
	return replaced, nil
}
