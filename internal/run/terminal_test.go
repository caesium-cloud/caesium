package run

import "testing"

func TestIsTerminal(t *testing.T) {
	terminal := []TaskStatus{
		TaskStatusSucceeded,
		TaskStatusFailed,
		TaskStatusSkipped,
		TaskStatusCached,
	}
	for _, s := range terminal {
		if !IsTerminal(s) {
			t.Errorf("IsTerminal(%q) = false, want true", s)
		}
	}

	nonTerminal := []TaskStatus{
		TaskStatusPending,
		TaskStatusRunning,
		TaskStatus("bogus"),
		TaskStatus(""),
	}
	for _, s := range nonTerminal {
		if IsTerminal(s) {
			t.Errorf("IsTerminal(%q) = true, want false", s)
		}
	}
}
