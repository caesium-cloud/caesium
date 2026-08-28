package tf

import (
	"encoding/json"
	"strings"
	"testing"
)

// terraform-exec's JSON commands end in a bare json.Decoder.Decode, so a
// response the decoder dislikes surfaces as an encoding/json error —
// and json.UnmarshalTypeError carries the offending VALUE for numbers. That
// error would flow to Emitter.FailClosed, to stderr, and into the persisted task
// log, which against `terraform output -json` (which prints sensitive values in
// full) defeats the withholding this package does immediately afterwards.
func TestSanitizeDecodeErrorWithholdsTheOffendingValue(t *testing.T) {
	var target struct {
		Secret string `json:"secret"`
	}
	err := json.Unmarshal([]byte(`{"secret": 8675309}`), &target)
	if err == nil {
		t.Fatal("expected an unmarshal type error")
	}
	if !strings.Contains(err.Error(), "8675309") {
		t.Skip("encoding/json no longer embeds the value; the sanitizer is belt-and-braces here")
	}

	sanitized := sanitizeDecodeError("terraform output in /src/stacks/network", err)
	if strings.Contains(sanitized.Error(), "8675309") {
		t.Fatalf("the offending value survived sanitization: %v", sanitized)
	}
	for _, want := range []string{"terraform output in /src/stacks/network", "secret", "value withheld"} {
		if !strings.Contains(sanitized.Error(), want) {
			t.Fatalf("sanitized error lost %q: %v", want, sanitized)
		}
	}
}

func TestSanitizeDecodeErrorKeepsSyntaxOffsetsAndPassesOtherErrorsThrough(t *testing.T) {
	var target map[string]any
	syntaxErr := json.Unmarshal([]byte("{not json"), &target)
	sanitized := sanitizeDecodeError("terraform show /src/tf.plan", syntaxErr)
	if !strings.Contains(sanitized.Error(), "byte offset") {
		t.Fatalf("a syntax error lost its offset, which is what makes it diagnosable: %v", sanitized)
	}

	// A Terraform failure (non-zero exit, diagnostics on stderr) must survive
	// intact: it is the operator's actual error message.
	plain := errTest("Error: Invalid provider configuration")
	if got := sanitizeDecodeError("terraform output", plain); !strings.Contains(got.Error(), "Invalid provider configuration") {
		t.Fatalf("a Terraform diagnostic was swallowed: %v", got)
	}
	if sanitizeDecodeError("op", nil) != nil {
		t.Fatal("sanitizeDecodeError(nil) should be nil")
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }
