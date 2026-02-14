package worker

import (
	"testing"
	"time"
)

func TestLeaseRenewInterval(t *testing.T) {
	tests := []struct {
		name string
		ttl  time.Duration
		want time.Duration
	}{
		{name: "default when ttl disabled", ttl: 0, want: defaultLeaseRenewInterval},
		{name: "half ttl", ttl: 20 * time.Second, want: 10 * time.Second},
		{name: "minimum bound", ttl: 1500 * time.Millisecond, want: minLeaseRenewInterval},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := leaseRenewInterval(tt.ttl)
			if got != tt.want {
				t.Fatalf("leaseRenewInterval(%s)=%s, want %s", tt.ttl, got, tt.want)
			}
		})
	}
}
