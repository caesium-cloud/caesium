//go:build !windows

package log

import (
	"bufio"
	"os"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"
)

var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// CaptureStderr redirects file descriptor 2 (stderr) through a pipe so that
// output from C libraries (e.g. libdqlite/libraft) is captured and routed
// through the structured logger instead of appearing as raw, colorized text.
//
// The original stderr fd is preserved via dup so that fatal/crash output from
// the Go runtime still reaches the terminal.
func CaptureStderr() error {
	// Preserve the original stderr fd for crash output.
	origFd, err := unix.Dup(int(os.Stderr.Fd()))
	if err != nil {
		return err
	}

	r, w, err := os.Pipe()
	if err != nil {
		unix.Close(origFd)
		return err
	}

	// Replace fd 2 with the write end of our pipe.
	if err := unix.Dup2(int(w.Fd()), int(os.Stderr.Fd())); err != nil {
		r.Close()
		w.Close()
		unix.Close(origFd)
		return err
	}
	w.Close() // fd 2 is now the write end; close the extra copy

	go func() {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			// Strip ANSI escape codes.
			line = ansiPattern.ReplaceAllString(line, "")
			if line == "" {
				continue
			}
			// Infer log level from C-layer output to preserve severity.
			lowerLine := strings.ToLower(line)
			switch {
			case strings.Contains(lowerLine, "error"):
				Error("dqlite/c", "msg", line)
			case strings.Contains(lowerLine, "warn"):
				Warn("dqlite/c", "msg", line)
			case strings.Contains(lowerLine, "info"):
				Info("dqlite/c", "msg", line)
			default:
				Debug("dqlite/c", "msg", line)
			}
		}
		// If the pipe breaks (e.g. during shutdown), restore stderr.
		unix.Dup2(origFd, 2)
		unix.Close(origFd)
	}()

	return nil
}
