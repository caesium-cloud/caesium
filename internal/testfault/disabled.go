//go:build !testfault

// Package testfault holds the single A1-approved, build-tag-gated test-only
// fault control: a predicate consulted after an execution event is durably
// committed and immediately before its first publication on the in-memory bus
// (see distributed-testing item B2 and A1's "Proxy and clock decision" record).
//
// This file is the RELEASE twin. It is the only version compiled unless the
// `testfault` build tag is supplied, and it deliberately contains:
//
//   - no listener, socket or HTTP route,
//   - no environment-variable name,
//   - no control-file path,
//   - no distinctive string literal of any kind.
//
// Enabled is an untyped constant `false`, so every `if testfault.Enabled { ... }`
// call site in product code is eliminated by the compiler before linking — the
// argument expressions are never evaluated and BeforeBusPublish itself is
// unreferenced and dropped by the linker. `scripts/robustness.sh` asserts the
// absence of the control's marker strings in the release image before it
// deploys it, so a release binary that ever gained the control fails closed.
package testfault

import "context"

// Enabled reports whether the test-only fault control is compiled in. It is an
// untyped constant so guarded blocks are removed at compile time.
const Enabled = false

// BeforeBusPublish is the release no-op. It is never called: every call site is
// guarded by `if testfault.Enabled`, which is constant false here.
func BeforeBusPublish(context.Context, string, string, string, uint64) {}
