package localrun

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/caesium-cloud/caesium/internal/run"
)

// failureLogTailLines is the most recent captured lines a local-run failure
// prints. The snapshot is already capped at pkgtask.MaxLogSnapshotBytes; this
// is a tighter tail so a chatty step does not flood the terminal.
const failureLogTailLines = 40

// failureLogTailBytes caps that tail independently of the line count, and
// stays under the persisted snapshot cap.
const failureLogTailBytes = 8 << 10

// formatRunFailure adds each failed step's name, attempt, exit code, and a
// bounded log tail to a local-run error. A nil cause is left nil. The cause
// itself is not rewritten: Runner.Run wraps it, RunWithResult returns it
// unchanged so harness errorContains checks keep matching the executor text.
// Log text is the persisted snapshot only. When LogScrubbed is set that
// snapshot is already the shared redaction path; this does not read a raw
// stream.
func formatRunFailure(cause error, result *RunResult) error {
	if cause == nil {
		return nil
	}
	detail := failedTaskDetail(result)
	if detail == "" {
		return cause
	}
	return fmt.Errorf("%w\n%s", cause, detail)
}

func failedTaskDetail(result *RunResult) string {
	if result == nil {
		return ""
	}
	var b strings.Builder
	for _, task := range result.Tasks {
		if task.Status != string(run.TaskStatusFailed) {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		name := task.Name
		if name == "" {
			name = task.TaskID.String()
		}
		fmt.Fprintf(&b, "step %s attempt %d", name, task.Attempt)
		if task.ExitCode != nil {
			fmt.Fprintf(&b, " exit %d", *task.ExitCode)
		}
		b.WriteByte('\n')
		tail := failureLogTail(task.LogText, task.LogTruncated)
		if tail == "" {
			b.WriteString("(no task log captured)")
			continue
		}
		if task.LogScrubbed {
			b.WriteString("log (scrubbed):\n")
		} else {
			b.WriteString("log:\n")
		}
		b.WriteString(tail)
	}
	return b.String()
}

// failureLogTail returns the end of a captured snapshot. snapshotTruncated
// means the capture itself stopped at the snapshot cap, so this tail is the
// end of what was kept, not necessarily the container's final bytes.
func failureLogTail(text string, snapshotTruncated bool) string {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	omitted := false
	if len(lines) > failureLogTailLines {
		lines = lines[len(lines)-failureLogTailLines:]
		omitted = true
	}
	tail := strings.Join(lines, "\n")
	if len(tail) > failureLogTailBytes {
		tail = tailBytes(tail, failureLogTailBytes)
		omitted = true
	}
	if !omitted && !snapshotTruncated {
		return tail
	}
	note := "[... showing log tail ...]"
	if snapshotTruncated {
		note = "[... captured log hit the snapshot cap; showing the end of what was kept ...]"
	}
	if tail == "" {
		return note
	}
	return note + "\n" + tail
}

func tailBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := len(s) - max
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	s = s[cut:]
	if i := strings.IndexByte(s, '\n'); i >= 0 && i+1 < len(s) {
		rest := s[i+1:]
		if strings.TrimRight(rest, "\n") != "" {
			s = rest
		}
	}
	return s
}
