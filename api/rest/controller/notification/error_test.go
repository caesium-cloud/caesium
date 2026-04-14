package notification

import (
	"testing"
)

func TestParseSafeOrderBy(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []string
		wantErr bool
	}{
		{"single column", "name", []string{"name asc"}, false},
		{"column with asc", "name asc", []string{"name asc"}, false},
		{"column with desc", "created_at desc", []string{"created_at desc"}, false},
		{"multiple columns", "name asc,created_at desc", []string{"name asc", "created_at desc"}, false},
		{"case insensitive direction", "name DESC", []string{"name desc"}, false},
		{"case insensitive column", "NAME", []string{"name asc"}, false},
		{"empty string", "", []string{}, false},
		{"unknown column", "password", nil, true},
		{"sql injection attempt", "name; DROP TABLE", nil, true},
		{"sql injection in column", "1=1--", nil, true},
		{"invalid direction", "name sideways", nil, true},
		{"too many tokens", "name asc extra", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSafeOrderBy(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d clauses, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("clause[%d]: got %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestMaskString(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"short", "****"},
		{"12345678", "****"},
		{"123456789", "1234****6789"},
		{"https://hooks.slack.com/services/T00/B00/xxxx", "http****xxxx"},
		{"sk-live-abcdefghijklmnop", "sk-l****mnop"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := maskString(tt.input)
			if got != tt.want {
				t.Errorf("maskString(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestRedactChannel_SensitiveKeys(t *testing.T) {
	// Verify that sensitive keys are masked in the response.
	ch := channelView{
		Config: map[string]interface{}{
			"url":         "https://example.com/hook",
			"password":    "supersecret123",
			"routing_key": "R012345678901234",
			"webhook_url": "https://hooks.slack.com/services/T00/B00/xxxx",
			"timeout":     "10s",
		},
	}

	// Simulate redaction by applying the same logic.
	for key, val := range ch.Config {
		if _, sensitive := sensitiveKeys[key]; sensitive {
			if s, ok := val.(string); ok && len(s) > 0 {
				ch.Config[key] = maskString(s)
			}
		}
	}

	// url should NOT be redacted (not in sensitiveKeys).
	if ch.Config["url"] != "https://example.com/hook" {
		t.Errorf("url should not be redacted, got %q", ch.Config["url"])
	}

	// password should be redacted.
	if ch.Config["password"] == "supersecret123" {
		t.Error("password should be redacted")
	}

	// routing_key should be redacted.
	if ch.Config["routing_key"] == "R012345678901234" {
		t.Error("routing_key should be redacted")
	}

	// webhook_url should be redacted.
	if ch.Config["webhook_url"] == "https://hooks.slack.com/services/T00/B00/xxxx" {
		t.Error("webhook_url should be redacted")
	}

	// timeout should NOT be redacted.
	if ch.Config["timeout"] != "10s" {
		t.Errorf("timeout should not be redacted, got %q", ch.Config["timeout"])
	}
}
