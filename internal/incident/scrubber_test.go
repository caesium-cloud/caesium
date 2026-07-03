package incident

import (
	"strings"
	"testing"
)

func TestScrubExactSecretRemoved(t *testing.T) {
	s := NewScrubber([]string{"hunter2-super-secret-value"})
	log := "connecting with password hunter2-super-secret-value to db"
	out := s.Scrub(log)
	if strings.Contains(out, "hunter2-super-secret-value") {
		t.Fatalf("secret value not removed: %q", out)
	}
	if !strings.Contains(out, Redacted) {
		t.Fatalf("expected redaction placeholder, got %q", out)
	}
}

// The load-bearing over-redaction guard test: a secret whose resolved value is
// "true" must NOT scrub every "true" in the log.
func TestScrubDoesNotOverRedactBooleanLiteral(t *testing.T) {
	s := NewScrubber([]string{"true"})
	log := "feature_enabled=true retries_exhausted=true status=true"
	out := s.Scrub(log)
	if out != log {
		t.Fatalf("boolean literal secret over-redacted the log.\n got: %q\nwant: %q", out, log)
	}
	if strings.Count(out, "true") != 3 {
		t.Fatalf("expected all 3 'true' tokens preserved, got %q", out)
	}
}

func TestScrubGuardsShortAndNumericAndDenylisted(t *testing.T) {
	// Short value, denylisted literal, and a bare number must all be skipped for
	// exact scrubbing.
	s := NewScrubber([]string{"abc", "false", "8080", "password"})
	log := "port 8080 flag false pass password prefix abc123"
	out := s.Scrub(log)
	if out != log {
		t.Fatalf("guarded values over-redacted: %q", out)
	}
}

func TestScrubHighEntropyToken(t *testing.T) {
	s := NewScrubber(nil)
	// A long, mixed, random-looking token should be redacted by the heuristic
	// even though it was not supplied as a known secret.
	token := "AKIAJ83HFKD9SLXMZ7Q2b8Xy1pQ9rT4"
	log := "aws credential " + token + " loaded"
	out := s.Scrub(log)
	if strings.Contains(out, token) {
		t.Fatalf("high-entropy token not redacted: %q", out)
	}
}

func TestScrubHighEntropyLeavesOrdinaryWords(t *testing.T) {
	s := NewScrubber(nil)
	log := "the quick brown fox jumps over the lazy dog repeatedly today"
	out := s.Scrub(log)
	if out != log {
		t.Fatalf("ordinary prose was redacted: %q", out)
	}
}

func TestScrubHighEntropyLeavesLongLowercaseWord(t *testing.T) {
	s := NewScrubber(nil)
	// Long, single-class (all lowercase) identifier must not be redacted.
	log := "processing supercalifragilisticexpialidocious path"
	out := s.Scrub(log)
	if out != log {
		t.Fatalf("long single-class word redacted: %q", out)
	}
}

func TestSecretValuesFromEnv(t *testing.T) {
	raw := map[string]string{
		"DB_PASSWORD": "secret://vault/db#password",
		"LOG_LEVEL":   "info",
		"API_KEY":     "secret://env/API_KEY",
	}
	resolved := map[string]string{
		"DB_PASSWORD": "s3cr3t-db-passphrase",
		"LOG_LEVEL":   "info",
		"API_KEY":     "ak-1234567890abcdef",
	}
	got := SecretValuesFromEnv(raw, resolved)
	if len(got) != 2 {
		t.Fatalf("expected 2 secret values, got %d: %v", len(got), got)
	}
	// The non-secret LOG_LEVEL value must not be included.
	for _, v := range got {
		if v == "info" {
			t.Fatalf("non-secret value leaked into scrub set: %v", got)
		}
	}
}
