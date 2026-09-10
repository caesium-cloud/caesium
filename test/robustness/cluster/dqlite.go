//go:build integration

package cluster

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/canonical/go-dqlite/v3/client"
)

type DqliteNode struct {
	ID       uint64
	Address  string
	Role     string
	IsLeader bool
}

type Membership struct {
	Leader  DqliteNode
	Members []DqliteNode
}

func (m Membership) LeaderAddress() string {
	return m.Leader.Address
}

func QueryNode(ctx context.Context, addr string) (leader *client.NodeInfo, members []client.NodeInfo, err error) {
	cli, err := client.New(ctx, addr)
	if err != nil {
		return nil, nil, fmt.Errorf("dqlite client %s: %w", addr, err)
	}
	defer func() { _ = cli.Close() }()

	leader, err = cli.Leader(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("dqlite Leader %s: %w", addr, err)
	}
	members, err = cli.Cluster(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("dqlite Cluster %s: %w", addr, err)
	}
	return leader, members, nil
}

func DiscoverMembership(ctx context.Context, addresses []string) (Membership, error) {
	if len(addresses) == 0 {
		return Membership{}, fmt.Errorf("no dqlite addresses")
	}
	type result struct {
		addr    string
		leader  *client.NodeInfo
		members []client.NodeInfo
	}
	results := make([]result, 0, len(addresses))
	var lastErr error
	for _, addr := range addresses {
		leader, members, err := QueryNode(ctx, addr)
		if err != nil {
			lastErr = err
			continue
		}
		results = append(results, result{addr: addr, leader: leader, members: members})
	}
	if len(results) != len(addresses) {
		if lastErr != nil {
			return Membership{}, fmt.Errorf("dqlite RPCs succeeded on %d/%d advertised members: %w", len(results), len(addresses), lastErr)
		}
		return Membership{}, fmt.Errorf("dqlite RPCs succeeded on %d/%d advertised members", len(results), len(addresses))
	}

	leaderAddr := ""
	if results[0].leader != nil {
		leaderAddr = results[0].leader.Address
	}
	if leaderAddr == "" {
		return Membership{}, fmt.Errorf("dqlite reported no leader")
	}
	for _, r := range results {
		got := ""
		if r.leader != nil {
			got = r.leader.Address
		}
		if got != leaderAddr {
			return Membership{}, fmt.Errorf("dqlite leader disagreement: %s vs %s", leaderAddr, got)
		}
	}

	seen := map[uint64]DqliteNode{}
	for _, n := range results[0].members {
		role := strings.ToLower(n.Role.String())
		seen[n.ID] = DqliteNode{
			ID:       n.ID,
			Address:  n.Address,
			Role:     role,
			IsLeader: n.Address == leaderAddr || (results[0].leader != nil && n.ID == results[0].leader.ID),
		}
	}
	if len(seen) != 3 {
		return Membership{}, fmt.Errorf("expected 3 distinct dqlite members, found %d", len(seen))
	}
	voters := 0
	var members []DqliteNode
	var leaderNode DqliteNode
	addrs := map[string]struct{}{}
	for _, n := range seen {
		if n.Address == "" {
			return Membership{}, fmt.Errorf("dqlite member %d has empty address", n.ID)
		}
		if _, dup := addrs[n.Address]; dup {
			return Membership{}, fmt.Errorf("duplicate dqlite address %s", n.Address)
		}
		addrs[n.Address] = struct{}{}
		if n.Role == "voter" {
			voters++
		}
		members = append(members, n)
		if n.IsLeader {
			leaderNode = n
		}
	}
	if voters != 3 {
		return Membership{}, fmt.Errorf("expected 3 dqlite voters, found %d", voters)
	}
	if leaderNode.Address == "" {
		return Membership{}, fmt.Errorf("agreed leader %s is not in the cluster list", leaderAddr)
	}
	return Membership{Leader: leaderNode, Members: members}, nil
}

func WaitMembership(ctx context.Context, addresses []string) (Membership, error) {
	var last error
	var found Membership
	err := Poll(ctx, time.Second, func() (bool, error) {
		m, err := DiscoverMembership(ctx, addresses)
		if err != nil {
			last = err
			return false, nil
		}
		found = m
		last = nil
		return true, nil
	})
	if err != nil {
		if last != nil {
			return Membership{}, fmt.Errorf("dqlite membership: %w (%v)", err, last)
		}
		return Membership{}, fmt.Errorf("dqlite membership: %w", err)
	}
	return found, nil
}

func HostIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}
