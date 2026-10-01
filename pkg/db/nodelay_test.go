package db

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNodeAddressPort(t *testing.T) {
	for _, tc := range []struct {
		address string
		port    int
		ok      bool
	}{
		{"127.0.0.1:9001", 9001, true},
		{"[::1]:9001", 9001, true},
		{"caesium-0.caesium.default.svc:9001", 9001, true},
		{"127.0.0.1", 0, false},
		{"127.0.0.1:0", 0, false},
		{"127.0.0.1:65536", 0, false},
		{"127.0.0.1:dqlite", 0, false},
	} {
		t.Run(fmt.Sprintf("%q", tc.address), func(t *testing.T) {
			port, err := nodeAddressPort(tc.address)
			if !tc.ok {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.port, port)
		})
	}
}
