package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWakeupURLForNodeAddress(t *testing.T) {
	u, err := WakeupURLForNodeAddress("10.0.0.12:9001", 8080)
	require.NoError(t, err)
	require.Equal(t, "http://10.0.0.12:8080/internal/wakeup", u)

	u, err = WakeupURLForNodeAddress("[fd00::12]:9001", 8080)
	require.NoError(t, err)
	require.Equal(t, "http://[fd00::12]:8080/internal/wakeup", u)
}

func TestCachedWakeupPeerResolverCachesWithinTTL(t *testing.T) {
	now := time.Unix(100, 0)
	var calls atomic.Int32

	resolver := &cachedWakeupPeerResolver{
		resolver: WakeupPeerResolverFunc(func(context.Context) ([]string, error) {
			if calls.Add(1) == 1 {
				return []string{"peer-a"}, nil
			}
			return []string{"peer-b"}, nil
		}),
		ttl: 5 * time.Second,
		now: func() time.Time {
			return now
		},
	}

	peers, err := resolver.WakeupPeers(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"peer-a"}, peers)

	peers[0] = "mutated"
	peers, err = resolver.WakeupPeers(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"peer-a"}, peers)
	require.Equal(t, int32(1), calls.Load())

	now = now.Add(6 * time.Second)
	peers, err = resolver.WakeupPeers(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"peer-b"}, peers)
	require.Equal(t, int32(2), calls.Load())
}

func TestDistributedWakeupsBroadcastFullFanout(t *testing.T) {
	const token = "shared-secret"
	var hits atomic.Int32

	serverA := wakeupTestServer(t, token, &hits)
	serverB := wakeupTestServer(t, token, &hits)

	d := NewDistributedWakeups(DistributedWakeupConfig{
		Token:      token,
		FanoutMode: WakeupFanoutFull,
		Resolver: WakeupPeerResolverFunc(func(context.Context) ([]string, error) {
			return []string{serverA.URL, serverB.URL}, nil
		}),
		HTTPClient: serverA.Client(),
	})

	d.broadcast(context.Background(), WakeupMessage{ID: "wakeup-1"})

	require.Eventually(t, func() bool {
		return hits.Load() == 2
	}, time.Second, 10*time.Millisecond)
}

func TestDistributedWakeupsHandleRemoteSignalsOncePerID(t *testing.T) {
	signaler := NewWakeupSignaler()
	d := NewDistributedWakeups(DistributedWakeupConfig{
		Token:      "shared-secret",
		FanoutMode: WakeupFanoutFull,
		Signaler:   signaler,
		Resolver: WakeupPeerResolverFunc(func(context.Context) ([]string, error) {
			return nil, nil
		}),
	})

	d.HandleRemote(context.Background(), WakeupMessage{ID: "wakeup-1"})
	assertSignal(t, signaler.C(), "first remote wakeup")

	d.HandleRemote(context.Background(), WakeupMessage{ID: "wakeup-1"})
	select {
	case <-signaler.C():
		t.Fatal("duplicate wakeup ID should not signal twice")
	case <-time.After(50 * time.Millisecond):
		// expected
	}
}

func TestDistributedWakeupsHandleRemoteGossipDetachesRequestContext(t *testing.T) {
	const token = "shared-secret"
	var hits atomic.Int32

	signaler := NewWakeupSignaler()
	server := wakeupTestServer(t, token, &hits)
	d := NewDistributedWakeups(DistributedWakeupConfig{
		Token:      token,
		FanoutMode: WakeupFanoutGossip,
		Signaler:   signaler,
		Resolver: WakeupPeerResolverFunc(func(ctx context.Context) ([]string, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return []string{server.URL}, nil
		}),
		HTTPClient: server.Client(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.HandleRemote(ctx, WakeupMessage{ID: "wakeup-ctx", TTL: 1})

	assertSignal(t, signaler.C(), "remote wakeup")
	require.Eventually(t, func() bool {
		return hits.Load() == 1
	}, time.Second, 10*time.Millisecond)
}

func TestDistributedWakeupsGossipSelectsLogarithmicFanout(t *testing.T) {
	d := NewDistributedWakeups(DistributedWakeupConfig{
		Token:      "shared-secret",
		FanoutMode: WakeupFanoutGossip,
	})

	peers := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	selected := d.selectPeers(peers)

	require.Len(t, selected, gossipFanout(len(peers)))
	require.Less(t, len(selected), len(peers))
}

func wakeupTestServer(t *testing.T, token string, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer "+token, r.Header.Get("Authorization"))
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	return server
}
