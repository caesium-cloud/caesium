package policy

import "sync/atomic"

// process is the holder this server process reads namespace settings from:
// the policy file's holder, or the env translation when no file is
// configured. cmd/start installs it once at startup.
var process atomic.Pointer[Holder]

// SetProcessHolder installs h as the process-wide policy holder. Packages
// outside internal/auth that need namespace settings (declared namespaces,
// secret rules, run quotas, Kubernetes targets) read it through
// ProcessHolder instead of having the holder threaded through every
// constructor. A nil h uninstalls it.
func SetProcessHolder(h *Holder) {
	process.Store(h)
}

// ProcessHolder returns the process-wide policy holder, or nil before
// cmd/start installs one (unit tests, offline CLI commands). Every Holder
// method accepts a nil receiver as the empty policy: only the default
// namespace is declared, no namespace has settings, and FileBacked is false.
func ProcessHolder() *Holder {
	return process.Load()
}
