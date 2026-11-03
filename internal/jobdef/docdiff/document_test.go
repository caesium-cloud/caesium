package docdiff

import (
	"fmt"
	"regexp"
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

var maskedValue = regexp.MustCompile(`\[REDACTED\] [0-9a-f]{8}`)

// A diff of masked documents holds no line of a credential, however long, while a
// change to one still shows. The key line sits outside the hunk here: the changed
// line of the value is far below it.
func TestMaskedDocumentsHoldNoLineOfACredential(t *testing.T) {
	var value strings.Builder
	for i := range 20 {
		fmt.Fprintf(&value, "      password-line-%02d\n", i)
	}
	before := "env:\n  DB_PASSWORD: |\n" + strings.ReplaceAll(value.String(), "      ", "    ") + "  REGION: eu\n"
	after := strings.Replace(before, "password-line-10", "password-line-XX", 1)

	complete, err := Documents([]byte(before), []byte(after))
	require.NoError(t, err)
	require.Contains(t, complete, "password-line-XX", "the complete diff keeps the value")

	masked, err := MaskedDocuments([]byte(before), []byte(after))
	require.NoError(t, err)
	require.NotContains(t, masked, "password-line")
	require.Len(t, maskedValue.FindAllString(masked, -1), 2, "diff:\n%s", masked)
	lines := maskedValue.FindAllString(masked, -1)
	require.NotEqual(t, lines[0], lines[1], "a changed credential still reads as a change")
}

func TestMaskedDocumentsShowNoChangeForAnEqualCredential(t *testing.T) {
	before := "env:\n  API_TOKEN: same-value\n  REGION: eu\n"
	masked, err := MaskedDocuments([]byte(before), []byte(strings.Replace(before, "REGION: eu", "REGION: us", 1)))
	require.NoError(t, err)
	require.Contains(t, masked, "REGION: us")
	require.Equal(t, 1, strings.Count(masked, "[REDACTED]"), "one unchanged context line, diff:\n%s", masked)
	require.NotRegexp(t, `(?m)^[-+].*REDACTED`, masked)
	require.NotContains(t, masked, "same-value")
}

// Two renders share no key, so a digest cannot be compared across renders.
func TestMaskedDigestsAreKeyedPerRender(t *testing.T) {
	document := []byte("API_TOKEN: same-value\n")
	first, err := MaskedDocument(document)
	require.NoError(t, err)
	second, err := MaskedDocument(document)
	require.NoError(t, err)
	require.Regexp(t, maskedValue, first)
	require.NotEqual(t, first, second)
}

func TestMaskedDocumentMasksByKey(t *testing.T) {
	for name, tc := range map[string]struct {
		document string
		kept     string
		hidden   string
	}{
		"password":       {document: "PASSWORD: abc", hidden: "abc"},
		"lower passwd":   {document: "db_passwd: abc", hidden: "abc"},
		"aws secret":     {document: "AWS_SECRET_ACCESS_KEY: abc", hidden: "abc"},
		"webhook secret": {document: "secret: abc", hidden: "abc"},
		"authorization":  {document: "Authorization: Bearer abc", hidden: "Bearer abc"},
		"api key":        {document: "api-key: abc", hidden: "abc"},
		"number":         {document: "PASSWORD: 123456", hidden: "123456"},
		"multi-line":     {document: "PRIVATE_KEY: |\n  line-one\n  line-two\n", hidden: "line-"},
		"folded":         {document: "TOKEN: >-\n  line-one\n  line-two\n", hidden: "line-"},
		"nested":         {document: "credentials:\n  user: abc\n", hidden: "abc"},
		"list":           {document: "tokens: [abc, def]", hidden: "abc"},
		"in a step list": {document: "steps:\n  - env:\n      PASSWORD: abc\n", hidden: "abc"},
		"reference":      {document: "TOKEN: secret://env/TOKEN", kept: "secret://env/TOKEN"},
		"quoted ref":     {document: `TOKEN: "secret://env/TOKEN"`, kept: "secret://env/TOKEN"},
		"boolean":        {document: "automountServiceAccountToken: true", kept: "automountServiceAccountToken: true"},
		"ordinary key":   {document: "REGION: eu", kept: "REGION: eu"},
		"image":          {document: "image: busybox:1.36.1", kept: "image: busybox:1.36.1"},
		"null":           {document: "PASSWORD: null", kept: "PASSWORD: null"},
	} {
		t.Run(name, func(t *testing.T) {
			masked, err := MaskedDocument([]byte(tc.document))
			require.NoError(t, err)
			if tc.hidden != "" {
				require.NotContains(t, masked, tc.hidden)
				require.Regexp(t, maskedValue, masked)
			}
			if tc.kept != "" {
				require.Contains(t, masked, tc.kept)
				require.NotContains(t, masked, "REDACTED")
			}
		})
	}
}

func TestMaskedDocumentsReportWhatTheyCannotRead(t *testing.T) {
	_, err := MaskedDocuments([]byte("a: [unterminated"), []byte("a: 1"))
	require.Error(t, err)
	_, err = MaskedDocument([]byte("a: [unterminated"))
	require.Error(t, err)
}
