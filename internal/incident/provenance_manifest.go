package incident

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"gopkg.in/yaml.v3"
)

// renderedManifest is a proposed definition as one YAML document that the real
// parser has accepted. Definition is that decode, so a stored manifest is
// compared against it on equal terms.
type renderedManifest struct {
	Definition *schema.Definition
	Document   []byte
}

// renderManifest renders a proposed definition and proves the document reads
// back, through the decoder git sync uses, as the job the human approved.
func renderManifest(definition json.RawMessage) (*renderedManifest, error) {
	var proposed schema.Definition
	if err := json.Unmarshal(definition, &proposed); err != nil {
		return nil, fmt.Errorf("decode definition: %w", err)
	}
	document, err := yaml.Marshal(&proposed)
	if err != nil {
		return nil, fmt.Errorf("render definition: %w", err)
	}
	if _, err := schema.Parse(document); err != nil {
		return nil, fmt.Errorf("parse rendered definition: %w", err)
	}
	var parsed schema.Definition
	if err := yaml.Unmarshal(document, &parsed); err != nil {
		return nil, fmt.Errorf("decode rendered definition: %w", err)
	}
	if !sameJob(&proposed, &parsed) {
		return nil, errors.New("the rendered document does not read back as the proposed definition")
	}
	return &renderedManifest{Definition: &parsed, Document: document}, nil
}

// sameJob compares two definitions field for field through their JSON form, the
// form a proposal arrives in. A blank namespace is `default`.
func sameJob(a, b *schema.Definition) bool {
	left, right := *a, *b
	left.Metadata.Namespace = models.NamespaceOrDefault(left.Metadata.Namespace)
	right.Metadata.Namespace = models.NamespaceOrDefault(right.Metadata.Namespace)
	leftJSON, err := json.Marshal(&left)
	if err != nil {
		return false
	}
	rightJSON, err := json.Marshal(&right)
	return err == nil && bytes.Equal(leftJSON, rightJSON)
}

// documentMarker is a `---` line: an optional comment may follow, content may not.
var documentMarker = regexp.MustCompile(`^---[ \t]*(#.*)?\r?$`)

// splitManifest cuts a stream at its `---` lines, each section keeping its own
// marker line, and joins back byte for byte. YAML forbids a marker at column zero
// inside a scalar, so this cannot split a value. Anything this does not model
// (directives, `...`, content on a marker line) is refused, not guessed at.
func splitManifest(raw []byte) ([][]byte, error) {
	var sections [][]byte
	start := 0
	lines := bytes.SplitAfter(raw, []byte("\n"))
	offset := 0
	for _, line := range lines {
		text := string(bytes.TrimRight(line, "\n"))
		switch {
		case documentMarker.MatchString(text):
			if offset > start {
				sections = append(sections, raw[start:offset])
			}
			start = offset
		case strings.HasPrefix(text, "---"), strings.HasPrefix(text, "..."), strings.HasPrefix(text, "%"):
			return nil, fmt.Errorf("unsupported document syntax %q", truncateLine(text))
		}
		offset += len(line)
	}
	if offset > start {
		sections = append(sections, raw[start:offset])
	}
	return sections, nil
}

func truncateLine(text string) string {
	if len(text) > 40 {
		return text[:40]
	}
	return text
}

// decodeManifest reads every document of a stream exactly as git sync does.
func decodeManifest(raw []byte) ([]schema.Definition, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var defs []schema.Definition
	for {
		var def schema.Definition
		err := dec.Decode(&def)
		if errors.Is(err, io.EOF) {
			return defs, nil
		}
		if err != nil {
			return nil, err
		}
		defs = append(defs, def)
	}
}

// leadingComments is the marker and comment header of a section, which belong to
// the file rather than to the mapping being replaced.
func leadingComments(section []byte) []byte {
	end := 0
	for _, line := range bytes.SplitAfter(section, []byte("\n")) {
		text := strings.TrimSpace(string(line))
		if text != "" && !strings.HasPrefix(text, "#") && !documentMarker.MatchString(strings.TrimRight(string(line), "\r\n")) {
			break
		}
		end += len(line)
	}
	return section[:end]
}

func unsafeManifest(format string, args ...any) *patchDegrade {
	return &patchDegrade{Code: degradeManifestUnsafe, Reason: "the manifest cannot be patched safely: " + fmt.Sprintf(format, args...)}
}

// splicedManifest is a manifest stream with one job's document replaced.
type splicedManifest struct {
	// File is the whole stream to write.
	File []byte
	// Original is the job's document as the file held it.
	Original []byte
}

// spliceManifest replaces one job's document in a manifest stream and leaves
// every other byte alone. The file must declare the job in the job's namespace:
// that read, from the repository itself, is what ties the job to this file. It
// returns a *patchDegrade when the change cannot be made safely, and
// ErrPatchAltersRemediation when it would change the policy stored in the file.
func spliceManifest(raw []byte, rendered *renderedManifest, alias, namespace string) (*splicedManifest, error) {
	sections, err := splitManifest(raw)
	if err != nil {
		return nil, unsafeManifest("%v", err)
	}
	before, err := decodeManifest(raw)
	if err != nil {
		return nil, unsafeManifest("the file does not parse")
	}

	target, documents := -1, 0
	for i, section := range sections {
		defs, err := decodeManifest(section)
		if err != nil || len(defs) > 1 {
			return nil, unsafeManifest("a document does not parse on its own")
		}
		if len(defs) == 0 {
			continue
		}
		if documents >= len(before) || !sameDefinition(&defs[0], &before[documents]) {
			return nil, unsafeManifest("its documents could not be located")
		}
		documents++
		if defs[0].Metadata.Alias != alias {
			continue
		}
		if models.NamespaceOrDefault(defs[0].Metadata.Namespace) != namespace {
			return nil, unsafeManifest("the file declares this alias in another namespace")
		}
		if target >= 0 {
			return nil, unsafeManifest("the file declares this alias more than once")
		}
		target = i
		if err := refuseManifestRemediationEdit(&defs[0], rendered.Definition); err != nil {
			return nil, err
		}
		if sameJob(&defs[0], rendered.Definition) {
			return nil, &patchDegrade{Code: degradeManifestUnchanged, Reason: "the manifest already declares this definition"}
		}
	}
	if documents != len(before) {
		return nil, unsafeManifest("its documents could not be located")
	}
	if target < 0 {
		return nil, unsafeManifest("the file does not declare this job")
	}

	var out bytes.Buffer
	for i, section := range sections {
		if i != target {
			out.Write(section)
			continue
		}
		out.Write(leadingComments(section))
		out.Write(rendered.Document)
	}
	updated := out.Bytes()

	after, err := decodeManifest(updated)
	if err != nil || len(after) != len(before) {
		return nil, unsafeManifest("the patched file does not parse")
	}
	for i := range after {
		if after[i].Metadata.Alias == alias {
			if !sameJob(&after[i], rendered.Definition) {
				return nil, unsafeManifest("the patched document does not read back as the approved one")
			}
			continue
		}
		if !sameDefinition(&after[i], &before[i]) {
			return nil, unsafeManifest("a sibling job would change")
		}
	}
	return &splicedManifest{File: updated, Original: sections[target]}, nil
}

// sameDefinition compares two parsed documents field for field.
func sameDefinition(a, b *schema.Definition) bool {
	left, err := yaml.Marshal(a)
	if err != nil {
		return false
	}
	right, err := yaml.Marshal(b)
	return err == nil && bytes.Equal(left, right)
}

// refuseManifestRemediationEdit extends refuseRemediationEdit to the file: the
// policy that governs the agent is the one in the repository, and a patch must
// not change it there either, whatever the stored job holds.
func refuseManifestRemediationEdit(stored, proposed *schema.Definition) error {
	current, err := canonicalRemediation(marshalRemediation(stored.Metadata.Remediation))
	if err != nil {
		return fmt.Errorf("incident: decode the manifest's remediation policy: %w", err)
	}
	next, err := canonicalRemediation(marshalRemediation(proposed.Metadata.Remediation))
	if err != nil {
		return fmt.Errorf("incident: encode proposed remediation policy: %w", err)
	}
	if !bytes.Equal(current, next) {
		return fmt.Errorf("%w (current %s, proposed %s)",
			ErrPatchAltersRemediation, remediationForMessage(current), remediationForMessage(next))
	}
	return nil
}
