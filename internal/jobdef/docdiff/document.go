// Package docdiff renders the difference between two YAML job documents. It sits
// apart from internal/jobdef/diff so internal/incident can use it: that package's
// tests reach internal/incident through internal/freshness.
package docdiff

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// maxDocumentCells bounds the line comparison, which needs a table of
// (lines before + 1) × (lines after + 1) cells once the common head and tail are
// set aside. Past it the caller shows the whole proposal, so a huge document
// cannot make a proposal expensive.
const maxDocumentCells = 4_000_000

// documentContext is how many unchanged lines frame each change.
const documentContext = 3

// ErrDocumentsTooLarge reports that two documents differ over too many lines to
// compare.
var ErrDocumentsTooLarge = errors.New("the documents differ over too many lines to compare")

// Documents renders the unified diff between two YAML job documents, and "" only
// when they hold the same data.
//
// internal/jobdef/diff's Compare reads the reduced JobSpec, which leaves out step
// env, mounts, edges and more. This reads the documents themselves, so nothing a document holds can be
// left out: each one decodes to plain data and re-encodes with sorted keys, and
// every line that differs is in the output. Comments, quoting and key order are
// not data, so they neither appear nor count as a change.
func Documents(current, proposed []byte) (string, error) {
	before, err := canonicalDocument(current)
	if err != nil {
		return "", fmt.Errorf("read the current document: %w", err)
	}
	after, err := canonicalDocument(proposed)
	if err != nil {
		return "", fmt.Errorf("read the proposed document: %w", err)
	}
	if before == after {
		return "", nil
	}
	script, err := lineEdits(strings.Split(before, "\n"), strings.Split(after, "\n"))
	if err != nil {
		return "", err
	}
	return renderUnified(script), nil
}

func canonicalDocument(document []byte) (string, error) {
	var data any
	if err := yaml.Unmarshal(document, &data); err != nil {
		return "", err
	}
	encoded, err := yaml.Marshal(data)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(encoded), "\n"), nil
}

// lineEdit is one line of an edit script: ' ' kept, '-' removed, '+' added.
type lineEdit struct {
	kind byte
	text string
}

// lineEdits is a longest-common-subsequence edit script from before to after.
func lineEdits(before, after []string) ([]lineEdit, error) {
	head := 0
	for head < len(before) && head < len(after) && before[head] == after[head] {
		head++
	}
	tail := 0
	for tail < len(before)-head && tail < len(after)-head &&
		before[len(before)-1-tail] == after[len(after)-1-tail] {
		tail++
	}
	removed, added := before[head:len(before)-tail], after[head:len(after)-tail]
	width := len(added) + 1
	if (len(removed)+1)*width > maxDocumentCells {
		return nil, ErrDocumentsTooLarge
	}

	// lengths[i*width+j] is the LCS length of removed[i:] and added[j:].
	lengths := make([]int32, (len(removed)+1)*width)
	for i := len(removed) - 1; i >= 0; i-- {
		for j := len(added) - 1; j >= 0; j-- {
			if removed[i] == added[j] {
				lengths[i*width+j] = lengths[(i+1)*width+j+1] + 1
			} else {
				lengths[i*width+j] = max(lengths[(i+1)*width+j], lengths[i*width+j+1])
			}
		}
	}

	script := make([]lineEdit, 0, len(before)+len(added))
	for _, line := range before[:head] {
		script = append(script, lineEdit{' ', line})
	}
	i, j := 0, 0
	for i < len(removed) && j < len(added) {
		switch {
		case removed[i] == added[j]:
			script = append(script, lineEdit{' ', removed[i]})
			i++
			j++
		case lengths[(i+1)*width+j] >= lengths[i*width+j+1]:
			script = append(script, lineEdit{'-', removed[i]})
			i++
		default:
			script = append(script, lineEdit{'+', added[j]})
			j++
		}
	}
	for ; i < len(removed); i++ {
		script = append(script, lineEdit{'-', removed[i]})
	}
	for ; j < len(added); j++ {
		script = append(script, lineEdit{'+', added[j]})
	}
	for _, line := range before[len(before)-tail:] {
		script = append(script, lineEdit{' ', line})
	}
	return script, nil
}

// renderUnified prints an edit script as unified diff hunks.
func renderUnified(script []lineEdit) string {
	var out strings.Builder
	out.WriteString("--- current\n+++ proposed\n")
	line, oldLine, newLine := 0, 1, 1
	advance := func(to int) {
		for ; line < to; line++ {
			if script[line].kind != '+' {
				oldLine++
			}
			if script[line].kind != '-' {
				newLine++
			}
		}
	}
	for next := 0; next < len(script); {
		if script[next].kind == ' ' {
			next++
			continue
		}
		last := next
		for i := next + 1; i < len(script) && i <= last+2*documentContext; i++ {
			if script[i].kind != ' ' {
				last = i
			}
		}
		start, end := max(next-documentContext, 0), min(last+documentContext+1, len(script))
		advance(start)
		oldStart, newStart := oldLine, newLine
		advance(end)
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", oldStart, oldLine-oldStart, newStart, newLine-newStart)
		for _, edit := range script[start:end] {
			out.WriteByte(edit.kind)
			out.WriteString(edit.text)
			out.WriteByte('\n')
		}
		next = end
	}
	return strings.TrimRight(out.String(), "\n")
}
