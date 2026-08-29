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
// is substituted as empty. The returned map is a copy; env is never mutated.
func InterpolateParamRefs(env map[string]string, params map[string]string) (map[string]string, error) {
	if len(env) == 0 {
		return env, nil
	}

	lookup := paramLookup(params)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make(map[string]string, len(env))
	var missing []string
	seenMissing := make(map[string]struct{})
	for _, key := range keys {
		interpolated, unresolved := interpolateParamRefsInValue(env[key], lookup)
		out[key] = interpolated
		for _, name := range unresolved {
			report := fmt.Sprintf("env %s: unresolved ${CAESIUM_PARAM_%s}", key, name)
			if _, ok := seenMissing[report]; ok {
				continue
			}
			seenMissing[report] = struct{}{}
			missing = append(missing, report)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("interpolate run params: %s", strings.Join(missing, "; "))
	}
	return out, nil
}

func paramLookup(params map[string]string) map[string]string {
	if len(params) == 0 {
		return nil
	}
	lookup := make(map[string]string, len(params))
	for k, v := range params {
		lookup[strings.ToUpper(k)] = v
	}
	return lookup
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
