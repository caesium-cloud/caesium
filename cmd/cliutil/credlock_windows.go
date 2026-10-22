//go:build windows

package cliutil

import "os"

// Windows has no flock; concurrent CLI processes there rely on the atomic
// rename alone (a later save can drop an earlier one's entry).
func lockFile(*os.File) error   { return nil }
func unlockFile(*os.File) error { return nil }
