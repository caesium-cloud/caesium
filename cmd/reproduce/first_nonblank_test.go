package reproduce

import "testing"

func TestFirstNonEmptyStringTrimsSelectedValue(t *testing.T) {
	if got := firstNonEmptyString(" ", " first ", "second"); got != "first" {
		t.Fatalf("selected = %q", got)
	}
	if got := firstNonEmptyString("", " "); got != "" {
		t.Fatalf("blank = %q", got)
	}
}
