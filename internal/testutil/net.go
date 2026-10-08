package testutil

import (
	"net"
	"testing"
)

// FreeLoopbackAddress returns an ephemeral loopback address after closing its
// listener. Server startup remains separate, so port allocation retains TOCTOU.
func FreeLoopbackAddress(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on loopback: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close loopback listener: %v", err)
	}
	return address
}
