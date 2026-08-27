package tf

import (
	"fmt"
	"sort"

	"github.com/hashicorp/terraform-exec/tfexec"
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
func PublishableOutputs(outputs map[string]tfexec.OutputMeta) (values map[string]string, withheld []string, err error) {
	values = make(map[string]string, len(outputs))
	names := make([]string, 0, len(outputs))
	for name := range outputs {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		meta := outputs[name]
		if meta.Sensitive {
			withheld = append(withheld, name)
			continue
		}
		value, err := OutputValue(meta)
		if err != nil {
			return nil, nil, fmt.Errorf("output %q: %w", name, err)
		}
		values[name] = value
	}
	return values, withheld, nil
}
