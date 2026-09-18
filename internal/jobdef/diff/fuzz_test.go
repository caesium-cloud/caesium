package diff

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"gopkg.in/yaml.v3"
)

// singleYAMLDocument reports whether data is EXACTLY one YAML document,
// determined independently of schema.Definition's own decode path via a
// generic yaml.Node walk (no isBlankDefinition, no Validate).
//
// This exists because len(viaDecode) == 1 does NOT mean "data is one
// document": decodeDefinitions silently skips a blank document
// (isBlankDefinition) before deciding what to hand its callback, so a
// TWO-document input where the first is blank ("{}") and the second is a
// valid manifest also yields exactly one accepted definition. schema.Parse
// (yaml.Unmarshal) only ever looks at the first document — "{}" — and
// rejects it (Validate fails on the empty apiVersion). Both behaviors are
// correct for what each function is documented to do; comparing them for
// that input compares two different questions, not a real disagreement.
// Gating the comparison on an independently-verified document count of
// exactly 1 is what makes "decodeDefinitions accepted one definition" and
// "schema.Parse looked at the same document" the same claim.
func singleYAMLDocument(data []byte) bool {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var first yaml.Node
	if err := dec.Decode(&first); err != nil {
		// Zero documents (empty/whitespace-only input), or the lone document
		// itself is not even syntactically parseable — neither is "exactly
		// one parsed document".
		return false
	}
	var second yaml.Node
	err := dec.Decode(&second)
	// io.EOF means the stream cleanly ended after exactly one document. Any
	// other outcome — a second document that parses (err == nil), or one
	// that doesn't (a non-EOF error) — means there is more stream after the
	// first document, so this is not "exactly one document" either way.
	return errors.Is(err, io.EOF)
}

func FuzzDecodeDefinitions(f *testing.F) {
	f.Add([]byte("apiVersion: v1\nkind: Job\nmetadata:\n  alias: test\ntrigger:\n  type: cron\n  configuration:\n    cron: \"* * * * *\"\nsteps:\n  - name: step1\n    engine: docker\n    image: alpine:3.23\n"))
	f.Add([]byte(""))
	f.Add([]byte("not yaml at all: [[["))
	f.Add([]byte("apiVersion: v1\nkind: Job\n"))
	// Regression for a reported false-positive: a blank first document
	// followed by a valid one. decodeDefinitions skips the blank doc and
	// yields exactly the second (len(viaDecode)==1, decodeErr==nil);
	// schema.Parse looks only at the blank first document and rejects it.
	// Both are correct; this must NOT be compared, and must pass.
	f.Add([]byte("{}\n---\napiVersion: v1\nkind: Job\nmetadata:\n  alias: test\ntrigger:\n  type: cron\n  configuration:\n    cron: \"* * * * *\"\nsteps:\n  - name: step1\n    engine: docker\n    image: alpine:3.23\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		tmp, err := os.CreateTemp("", "fuzz-def-*.yaml")
		if err != nil {
			t.Skip("cannot create temp file")
		}
		defer os.Remove(tmp.Name())
		if _, err := tmp.Write(data); err != nil {
			tmp.Close()
			t.Skip("cannot write temp file")
		}
		tmp.Close()

		var viaDecode []schema.Definition
		decodeErr := decodeDefinitions(tmp.Name(), func(def *schema.Definition) error {
			viaDecode = append(viaDecode, *def)
			return nil
		})

		// Consistency with schema.Parse: decodeDefinitions
		// (internal/jobdef/diff, the loader behind `job diff`/`job apply`) and
		// schema.Parse (pkg/jobdef, single-document YAML decode + Validate)
		// are two independent implementations that both bottom out in the
		// same schema.Definition.Validate — one via a streaming
		// yaml.Decoder, the other via yaml.Unmarshal. For a single-document
		// input they answer the same question ("is this one valid job
		// definition?") and must agree, or `job apply` and `job lint`'s
		// underlying parser would silently accept different manifests as
		// valid.
		//
		// The comparison is scoped to the case where both paths are actually
		// answering that same question AND data is independently confirmed to
		// be exactly one YAML document (singleYAMLDocument, above) — a
		// len(viaDecode)==1 count is NOT sufficient on its own: decodeDefinitions
		// silently skips a blank document via isBlankDefinition before deciding
		// what to hand its callback, so a two-document input whose first
		// document is blank and second is a valid manifest ALSO yields exactly
		// one accepted definition, even though schema.Parse (which only ever
		// looks at the first document) is answering an entirely different
		// question — this was a real false-positive this fuzz target reported;
		// see the regression seed above.
		//
		//   - single document, decodeDefinitions accepted exactly one
		//     definition: schema.Parse over the identical bytes must accept it
		//     too.
		//   - single document, decodeDefinitions rejected it before producing
		//     any definition: schema.Parse must reject it too.
		// Left uncompared, for a stated structural reason rather than a
		// convenient skip:
		//   - not exactly one document (zero, or two-or-more) — schema.Parse's
		//     "look only at the first document" contract and
		//     decodeDefinitions' "walk every document, skip blanks" contract
		//     are not answering the same question.
		//   - a genuinely single blank document (zero definitions with no
		//     error) is decodeDefinitions' documented isBlankDefinition special
		//     case, which schema.Parse has no equivalent of.
		if singleYAMLDocument(data) {
			parsed, parseErr := schema.Parse(data)
			switch {
			case decodeErr == nil && len(viaDecode) == 1:
				if parseErr != nil {
					t.Fatalf("decodeDefinitions accepted a single-document manifest that schema.Parse rejected: decoded=%+v parse_err=%v\ninput=%q",
						viaDecode[0], parseErr, data)
				}
			case decodeErr != nil && len(viaDecode) == 0:
				if parseErr == nil {
					t.Fatalf("decodeDefinitions rejected input that schema.Parse accepted: decode_err=%v parsed=%+v\ninput=%q",
						decodeErr, parsed, data)
				}
			}
		}
	})
}
