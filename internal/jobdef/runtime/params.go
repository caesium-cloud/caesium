package runtime

import (
	"github.com/google/uuid"
	"strings"
)

// BuildRunParamEnv includes the two run identity entries even without params.
// Keys that collide after uppercasing retain their existing unspecified winner.
func BuildRunParamEnv(runID uuid.UUID, jobAlias string, params map[string]string) map[string]string {
	env := make(map[string]string, len(params)+2)
	env["CAESIUM_RUN_ID"] = runID.String()
	env["CAESIUM_JOB_ALIAS"] = jobAlias
	for key, value := range params {
		env["CAESIUM_PARAM_"+strings.ToUpper(key)] = value
	}
	return env
}
