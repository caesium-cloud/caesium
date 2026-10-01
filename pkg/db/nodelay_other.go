//go:build !linux

package db

// setNodeNoDelay is a no-op off Linux: the embedded dqlite node, and the
// delayed-ACK interaction it works around, are Linux-only.
func setNodeNoDelay(int) (int, error) { return 0, nil }
