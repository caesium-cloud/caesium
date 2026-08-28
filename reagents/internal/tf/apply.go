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
// It also refuses to publish a name that cannot survive the environment
// transport a consumer reads it through — see CheckOutputNames. That check is
// here, at the producing end, because it is the only place the operator is
// looking at the output whose name has to change.
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
	// Before any of them is published: a name that cannot survive the transport
	// is a wrong deployment waiting to happen, and it must be rejected while the
	// operator can still rename it.
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

// CheckOutputNames refuses output names that do not survive the environment
// transport intact.
//
// The cross-stack wiring travels as environment variables, and that trip is
// LOSSY. Caesium spells an output key as CAESIUM_OUTPUT_<STEP>_<KEY> by
// uppercasing it and mapping "-" and "." to "_" (pkg/task.BuildOutputEnv), and
// the importing side lowercases the suffix back to a Terraform variable name
// (tf-runner's exportImportedOutputs). So:
//
//	vpc_id  -> VPC_ID  -> vpc_id   round-trips
//	vpcId   -> VPCID   -> vpcid    arrives under a DIFFERENT name
//	vpc-id  -> VPC_ID  -> vpc_id   arrives under a different name, and collides
//	                               with a sibling output actually named vpc_id
//
// None of those failures is visible at runtime: an undeclared TF_VAR_ is
// silently ignored by Terraform (see ExportVariable), so the consumer plans
// against the variable's default and the run is green with the wrong inputs. A
// collision is worse still — which of the two values wins is decided by
// os.Environ() ordering.
//
// The transport cannot be made lossless from this side (the naming rule belongs
// to Caesium, and the reagents must not grow a second copy of it), so the contract
// is stated as a restriction instead: a published output name is lowercase
// [a-z0-9_]. That is exactly the fixed set of the round trip, and it is a
// subset of what Terraform already allows an output to be called, so complying
// is always a rename rather than a redesign.
func CheckOutputNames(names []string) error {
	// Collisions first: two names that fold together produce the more useful
	// error, because it names both outputs rather than one of them twice.
	folded := make(map[string]string, len(names))
	for _, name := range names {
		key := envRoundTrip(name)
		if prior, dup := folded[key]; dup {
			return fmt.Errorf(
				"outputs %q and %q both reach a consumer as TF_VAR_%s: Caesium uppercases an output key and maps "+
					"\"-\" and \".\" to \"_\", so their names are indistinguishable after the trip through the "+
					"environment and which value wins is undefined. Rename one of them",
				prior, name, key)
		}
		folded[key] = name
	}
	for _, name := range names {
		if got := envRoundTrip(name); got != name {
			return fmt.Errorf(
				"output %q reaches a consumer as TF_VAR_%s, not TF_VAR_%s: Caesium uppercases an output key and "+
					"maps \"-\" and \".\" to \"_\" on the way out, and the importing step lowercases it on the way "+
					"in. Rename the output using only lowercase letters, digits and underscores so it survives "+
					"the trip",
				name, got, name)
		}
		if err := publishableName(name); err != nil {
			return err
		}
	}
	return nil
}

// envRoundTrip is what an output name becomes after Caesium spells it into an
// environment variable and the importing step spells it back out.
func envRoundTrip(name string) string {
	replaced := strings.NewReplacer("-", "_", ".", "_").Replace(name)
	return strings.ToLower(strings.ToUpper(replaced))
}

// publishableName rejects anything the round trip leaves alone but that is not
// a legal environment variable name — non-ASCII being the obvious one, since
// upper/lower of a rune outside [a-zA-Z] can be the identity.
func publishableName(name string) error {
	if name == "" {
		return fmt.Errorf("an output with an empty name cannot be published")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
		default:
			return fmt.Errorf(
				"output %q contains %q, which cannot appear in the environment variable a consumer reads it from; "+
					"rename it using lowercase letters, digits and underscores",
				name, r)
		}
	}
	return nil
}
