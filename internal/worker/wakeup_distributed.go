package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
)

const (
	WakeupFanoutFull   = "full"
	WakeupFanoutGossip = "gossip"

	defaultWakeupHTTPTimeout = 750 * time.Millisecond
	defaultWakeupGossipTTL   = 3
	maxSeenWakeupIDs         = 2048
	seenWakeupIDTTL          = 5 * time.Minute
)

type WakeupPeerResolver interface {
	WakeupPeers(ctx context.Context) ([]string, error)
}

type WakeupPeerResolverFunc func(context.Context) ([]string, error)

func (f WakeupPeerResolverFunc) WakeupPeers(ctx context.Context) ([]string, error) {
	return f(ctx)
}

func WakeupURLForNodeAddress(nodeAddress string, apiPort int) (string, error) {
	if apiPort <= 0 {
		apiPort = 8080
	}

	host, _, err := net.SplitHostPort(nodeAddress)
	if err != nil {
		host = strings.TrimSpace(nodeAddress)
	}
	if host == "" {
		return "", fmt.Errorf("empty host in node address %q", nodeAddress)
	}
	if strings.HasPrefix(host, "@") {
		return "", fmt.Errorf("abstract unix dqlite address %q cannot be used for HTTP wakeups", nodeAddress)
	}

	return (&url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(host, strconv.Itoa(apiPort)),
		Path:   "/internal/wakeup",
	}).String(), nil
}

type DistributedWakeupConfig struct {
	Token      string
	FanoutMode string
	Signaler   *WakeupSignaler
	Resolver   WakeupPeerResolver
	HTTPClient *http.Client
}

type DistributedWakeups struct {
	token      string
	mode       string
	signaler   *WakeupSignaler
	resolver   WakeupPeerResolver
	httpClient *http.Client

	seenMu sync.Mutex
	seen   map[string]time.Time
	now    func() time.Time
}

type WakeupMessage struct {
	ID  string `json:"id,omitempty"`
	TTL int    `json:"ttl,omitempty"`
}

func NewDistributedWakeups(cfg DistributedWakeupConfig) *DistributedWakeups {
	signaler := cfg.Signaler
	if signaler == nil {
		signaler = NewWakeupSignaler()
	}
	resolver := cfg.Resolver
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultWakeupHTTPTimeout}
	}

	return &DistributedWakeups{
		token:      strings.TrimSpace(cfg.Token),
		mode:       normalizeWakeupFanoutMode(cfg.FanoutMode),
		signaler:   signaler,
		resolver:   resolver,
		httpClient: client,
		seen:       make(map[string]time.Time),
		now:        time.Now,
	}
}

func normalizeWakeupFanoutMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case WakeupFanoutGossip:
		return WakeupFanoutGossip
	default:
		return WakeupFanoutFull
	}
}

func (d *DistributedWakeups) Start(ctx context.Context, bus event.Bus) error {
	if d == nil || bus == nil || d.token == "" {
		return nil
	}

	events, err := bus.Subscribe(ctx, event.Filter{Types: wakeupEventTypes()})
	if err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-events:
			if !ok {
				return nil
			}
			id := uuid.NewString()
			d.remember(id)
			ttl := 0
			if d.mode == WakeupFanoutGossip {
				ttl = defaultWakeupGossipTTL
			}
			go d.broadcast(ctx, WakeupMessage{ID: id, TTL: ttl})
		}
	}
}

func (d *DistributedWakeups) HandleRemote(ctx context.Context, msg WakeupMessage) {
	if d == nil {
		return
	}
	if msg.ID == "" {
		msg.ID = uuid.NewString()
	}
	if !d.remember(msg.ID) {
		return
	}

	d.signaler.Signal()
	if d.mode == WakeupFanoutGossip && msg.TTL > 0 {
		msg.TTL--
		go d.broadcast(ctx, msg)
	}
}

func (d *DistributedWakeups) broadcast(ctx context.Context, msg WakeupMessage) {
	if d == nil || d.token == "" || d.resolver == nil {
		return
	}

	peers, err := d.resolver.WakeupPeers(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("failed to resolve wakeup peers", "error", err)
		}
		return
	}
	peers = d.selectPeers(peers)
	if len(peers) == 0 {
		return
	}

	var wg sync.WaitGroup
	for _, peer := range peers {
		peer := peer
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.postWakeup(ctx, peer, msg)
		}()
	}
	wg.Wait()
}

func (d *DistributedWakeups) selectPeers(peers []string) []string {
	if len(peers) == 0 {
		return nil
	}
	selected := append([]string(nil), peers...)
	if d.mode != WakeupFanoutGossip {
		return selected
	}

	for i := len(selected) - 1; i > 0; i-- {
		j := rand.IntN(i + 1)
		selected[i], selected[j] = selected[j], selected[i]
	}
	limit := gossipFanout(len(selected))
	if limit >= len(selected) {
		return selected
	}
	return selected[:limit]
}

func gossipFanout(peerCount int) int {
	if peerCount <= 1 {
		return peerCount
	}
	return int(math.Ceil(math.Log2(float64(peerCount)))) + 1
}

func (d *DistributedWakeups) postWakeup(ctx context.Context, peer string, msg WakeupMessage) {
	body, err := json.Marshal(msg)
	if err != nil {
		log.Warn("failed to marshal wakeup message", "error", err)
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, peer, bytes.NewReader(body))
	if err != nil {
		log.Warn("failed to create wakeup request", "peer", peer, "error", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := d.httpClient.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			log.Debug("distributed wakeup failed", "peer", peer, "error", err)
		}
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Debug("distributed wakeup returned non-success status", "peer", peer, "status", resp.StatusCode)
	}
}

func (d *DistributedWakeups) remember(id string) bool {
	if id == "" {
		return true
	}

	d.seenMu.Lock()
	defer d.seenMu.Unlock()

	now := d.now()
	if seenAt, ok := d.seen[id]; ok && now.Sub(seenAt) < seenWakeupIDTTL {
		return false
	}
	d.seen[id] = now

	if len(d.seen) > maxSeenWakeupIDs {
		for seenID, seenAt := range d.seen {
			if now.Sub(seenAt) >= seenWakeupIDTTL || len(d.seen) > maxSeenWakeupIDs {
				delete(d.seen, seenID)
			}
			if len(d.seen) <= maxSeenWakeupIDs {
				break
			}
		}
	}
	return true
}
