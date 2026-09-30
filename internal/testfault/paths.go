package testfault

// PathDispatchOnce and PathPublishAndMark identify the two publication paths
// A1 requires the test-only predicate to cover, so the background dispatcher
// can never bypass an armed pause.
//
// They live in an untagged file because the call sites in
// internal/event/bus_dispatch.go name them in both builds. They are untyped
// constants used only inside `if testfault.Enabled { ... }`, which is a false
// constant in release builds, so the compiler removes those blocks and emits
// neither string. `scripts/robustness.sh` verifies that against the built
// release binary rather than trusting this comment.
const (
	PathDispatchOnce   = "dispatch_once"
	PathPublishAndMark = "publish_and_mark"
)
