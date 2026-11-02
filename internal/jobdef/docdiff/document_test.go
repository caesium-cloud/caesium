package docdiff

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const documentBase = `apiVersion: v1
kind: Job
metadata:
  alias: orders
trigger:
  type: cron
  configuration:
    cron: "0 6 * * *"
steps:
  - name: load
    image: busybox:1.36.1
    env:
      REGION: eu
    mounts:
      - type: bind
        source: /data
        target: /in
    next: [publish]
  - name: publish
    image: busybox:1.36.1
`

// Every field a document holds is compared, including the ones the reduced
// JobSpec leaves out.
func TestDocumentsShowFieldsTheReducedSpecOmits(t *testing.T) {
	for name, edit := range map[string][2]string{
		"env":        {"REGION: eu", "REGION: us"},
		"mount":      {"target: /in", "target: /out"},
		"edge":       {"next: [publish]", "next: []"},
		"step image": {"- name: publish\n    image: busybox:1.36.1", "- name: publish\n    image: alpine:3.23"},
	} {
		t.Run(name, func(t *testing.T) {
			text, err := Documents([]byte(documentBase), []byte(strings.Replace(documentBase, edit[0], edit[1], 1)))
			require.NoError(t, err)
			require.NotEmpty(t, text, "the edit is not reported")
			require.Contains(t, text, "\n+")
			require.Contains(t, text, "\n-")
		})
	}
}

// Comments, quoting and key order are not data: "" means the data is identical.
func TestDocumentsIgnoreFormatting(t *testing.T) {
	reformatted := "# a comment\nkind: Job\napiVersion: 'v1'\n" +
		strings.TrimPrefix(documentBase, "apiVersion: v1\nkind: Job\n")
	text, err := Documents([]byte(documentBase), []byte(reformatted))
	require.NoError(t, err)
	require.Empty(t, text)
}

func TestDocumentsRenderAUnifiedDiff(t *testing.T) {
	before := "a: 1\nb: 2\nc: 3\nd: 4\ne: 5\nf: 6\n"
	after := "a: 1\nb: 2\nc: 3\nd: 40\ne: 5\nf: 6\ng: 7\n"
	text, err := Documents([]byte(before), []byte(after))
	require.NoError(t, err)
	require.Equal(t, strings.Join([]string{
		"--- current",
		"+++ proposed",
		"@@ -1,6 +1,7 @@",
		" a: 1",
		" b: 2",
		" c: 3",
		"-d: 4",
		"+d: 40",
		" e: 5",
		" f: 6",
		"+g: 7",
	}, "\n"), text)
}

func TestDocumentsSplitDistantChangesIntoHunks(t *testing.T) {
	var before, after strings.Builder
	for i := range 40 {
		fmt.Fprintf(&before, "k%02d: same\n", i)
		value := "same"
		if i == 2 || i == 30 {
			value = "changed"
		}
		fmt.Fprintf(&after, "k%02d: %s\n", i, value)
	}
	text, err := Documents([]byte(before.String()), []byte(after.String()))
	require.NoError(t, err)
	require.Contains(t, text, "@@ -1,6 +1,6 @@\n k00: same\n k01: same\n-k02: same\n+k02: changed\n k03: same\n")
	require.Contains(t, text, "@@ -28,7 +28,7 @@\n k27: same\n k28: same\n k29: same\n-k30: same\n+k30: changed\n k31: same\n")
	require.NotContains(t, text, "k15")
}

func TestDocumentsReportWhatTheyCannotRead(t *testing.T) {
	_, err := Documents([]byte("a: [unterminated"), []byte("a: 1"))
	require.Error(t, err)
	_, err = Documents([]byte("a: 1"), []byte("a: [unterminated"))
	require.Error(t, err)
}

// A comparison past the bound refuses, so the caller shows the proposal rather
// than a claim about it.
func TestDocumentsRefuseAComparisonPastTheBound(t *testing.T) {
	var before, after strings.Builder
	for i := range 3000 {
		fmt.Fprintf(&before, "a%d: 1\n", i)
		fmt.Fprintf(&after, "b%d: 2\n", i)
	}
	_, err := Documents([]byte(before.String()), []byte(after.String()))
	require.ErrorIs(t, err, ErrDocumentsTooLarge)
}
