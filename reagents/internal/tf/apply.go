package tf

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-exec/tfexec"
	tfjson "github.com/hashicorp/terraform-json"
)

// PublishableOutputs renders the root module's outputs as the scalar map a
// Caesium step may emit, and returns the names of the outputs it withheld.
//
// Dropping every `sensitive = true` output is the first of design §6.4's two
// security requirements, and it is not cosmetic. A step's structured output is
// persisted in dqlite, served by the tasks API, replayed on a cache hit, shown
// by `caesium why --json`, and injected into every downstream container as an
// environment variable — so an emitted secret is a secret in six places at once.
// Secrets belong in a secret store, reached through Caesium's `secret://`
// providers, and never travel as step outputs.
//
// The Sensitive flag comes from terraform-json's typed OutputMeta rather than
// from parsing `terraform output` text, which is what makes this a property of
// the type system instead of a convention someone can forget (design §6.7).
//
// It also refuses to publish a name a consumer could not import exactly — see
// CheckOutputNames. That check is here, at the producing end, because a
// collision or an illegal identifier is a wrong deployment waiting to happen,
// and this is the only place the operator is looking at the output whose name
// has to change.
func PublishableOutputs(outputs map[string]tfexec.OutputMeta) (values map[string]string, withheld []string, err error) {
	values = make(map[string]string, len(outputs))
	names := make([]string, 0, len(outputs))
	for name := range outputs {
		names = append(names, name)
	}
	sort.Strings(names)

	publishable := make([]string, 0, len(names))
	for _, name := range names {
		if outputs[name].Sensitive {
			withheld = append(withheld, name)
			continue
		}
		publishable = append(publishable, name)
	}
	// Before any of them is published: two names that fold onto one env var, or
	// a name that is not a Terraform identifier, cannot be imported exactly,
	// and must be rejected while the operator can still rename them.
	if err := CheckOutputNames(publishable); err != nil {
		return nil, nil, err
	}

	for _, name := range publishable {
		value, err := OutputValue(outputs[name])
		if err != nil {
			return nil, nil, fmt.Errorf("output %q: %w", name, err)
		}
		values[name] = value
	}
	return values, withheld, nil
}

// PlannedPublishableOutputNames lists the root outputs a SAVED PLAN says it
// would publish, so their names can be checked before anything is applied.
//
// The naming rule is enforced twice on purpose. This is the early pass: it runs
// against the reviewed plan artifact, before `terraform apply` mutates
// anything, so a stack whose output name cannot reach a consumer fails while
// nothing has been deployed. PublishableOutputs remains the authoritative pass,
// because only `terraform output` carries terraform-json's typed
// OutputMeta.Sensitive and only it sees an output the plan reports as unchanged.
//
// Sensitivity is therefore read CONSERVATIVELY here: a name is returned only
// when the plan states plainly that it is not sensitive (after_sensitive is the
// boolean false). Anything else — true, absent, or a shape this does not
// recognise — is left out. Guessing "sensitive" costs a failure one phase later
// against the authoritative check; guessing "publishable" would fail a correct
// deployment on a value that was never going to be published at all.
func PlannedPublishableOutputNames(plan *tfjson.Plan) []string {
	if plan == nil {
		return nil
	}
	names := make([]string, 0, len(plan.OutputChanges))
	for name, change := range plan.OutputChanges {
		if change == nil {
			continue
		}
		notSensitive, ok := change.AfterSensitive.(bool)
		if !ok || notSensitive {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// OutputNamesIndexEnvPrefix is the dedicated environment prefix Caesium uses
// for the generated name index: CAESIUM_OUTPUT_NAME_INDEX_<STEP>. Locked to
// pkg/task.OutputNamesIndexEnvPrefix.
const OutputNamesIndexEnvPrefix = "CAESIUM_OUTPUT_NAME_INDEX_"

// OutputNamesIndexEnv is the dedicated env var carrying the generated JSON
// name index for step.
func OutputNamesIndexEnv(step string) string {
	return OutputNamesIndexEnvPrefix + foldOutputKey(step)
}

// OutputNamesIndexRequiredKey is the tf-apply protocol sentinel saying this
// output row contains at least one name that cannot be recovered by the
// historical lowercase-suffix rule. A new tf-runner that sees the sentinel
// requires Caesium to have supplied OutputNamesIndexEnv(step); an old server
// therefore fails explicitly instead of silently importing vpcId as vpcid.
//
// The sentinel is persisted in the ordinary output row specifically so old
// servers carry it. It contains no name metadata and cannot be mistaken for a
// user-provided JSON index.
const OutputNamesIndexRequiredKey = "caesium_output_name_index_required"

// OutputNamesNeedIndex reports whether any output name needs the dedicated
// server-generated index to survive the environment fold.
func OutputNamesNeedIndex(outputs map[string]string) bool {
	for name := range outputs {
		if !outputNameSurvivesFold(name) {
			return true
		}
	}
	return false
}

func outputNameSurvivesFold(name string) bool {
	return strings.ToLower(foldOutputKey(name)) == name
}

// foldOutputKey is what Caesium spells an output key as inside an environment
// variable (pkg/task.NormalizeStepName). The reagents restate it rather than
// import Caesium; test/infra_deploy_test.go drives the real server.
func foldOutputKey(name string) string {
	return strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(name))
}

// CheckOutputNames refuses output names a consumer could not import exactly.
//
// The cross-stack wiring travels as environment variables. Caesium spells an
// output key as CAESIUM_OUTPUT_<STEP>_<KEY> by uppercasing it and mapping "-"
// and "." to "_" (pkg/task.BuildOutputEnv). That fold is not injective:
//
//	vpc_id  -> VPC_ID   unique
//	vpcId   -> VPCID    unique, but the historical lowercase recovery would
//	                    have arrived as vpcid
//	vpc-id  -> VPC_ID   collides with a sibling actually named vpc_id
//
// Caesium's dedicated JSON name index makes the unique cases lossless —
// IMPORT_OUTPUTS_FROM restores the original name — so mixed case and dashes
// are allowed. What this still refuses:
//
//   - two names that fold onto the same env suffix: the index cannot
//     disambiguate two values in one variable, and which one wins would be
//     os.Environ() ordering
//   - a name that is not a Terraform identifier (dots, non-ASCII, a leading
//     digit): it cannot be exported as TF_VAR_<name> (see ExportVariable)
//
// An undeclared TF_VAR_ is silently ignored by Terraform, so either failure
// would otherwise be a green consumer planned against variable defaults.
func CheckOutputNames(names []string) error {
	// Collisions first: two names that fold together produce the more useful
	// error, because it names both outputs rather than one of them twice.
	folded := make(map[string]string, len(names))
	for _, name := range names {
		key := foldOutputKey(name)
		if prior, dup := folded[key]; dup {
			return fmt.Errorf(
				"outputs %q and %q both fold to the environment suffix %s: Caesium uppercases an output key and maps "+
					"\"-\" and \".\" to \"_\", so their values cannot both reach a consumer and which one wins is "+
					"undefined. Rename one of them",
				prior, name, key)
		}
		folded[key] = name
	}
	for _, name := range names {
		if name == OutputNamesIndexRequiredKey {
			return fmt.Errorf("output %q is reserved by tf-apply; rename it", name)
		}
		if err := publishableName(name); err != nil {
			return err
		}
	}
	return nil
}

// publishableName requires a Terraform identifier. Mixed case and hyphens are
// allowed — the name index carries them through the environment fold. Dots,
// spaces and non-ASCII are not, because they cannot appear in TF_VAR_<name>.
func publishableName(name string) error {
	if name == "" {
		return fmt.Errorf("an output with an empty name cannot be published")
	}
	if validVariableName(name) {
		return nil
	}
	return fmt.Errorf(
		"output %q is not a Terraform identifier, so it cannot be imported as TF_VAR_%s; "+
			"rename it using letters, digits, underscores and hyphens (not starting with a digit or hyphen)",
		name, name)
}
