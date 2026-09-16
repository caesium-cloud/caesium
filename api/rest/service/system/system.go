package system

import (
	"context"
	"runtime"
	"sort"
	"strings"
	"time"

	contractsvc "github.com/caesium-cloud/caesium/api/rest/service/contract"
	clustersvc "github.com/caesium-cloud/caesium/internal/cluster"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	"gorm.io/gorm"
)

type NodeRole string

const (
	RoleVoter   NodeRole = "voter"
	RoleStandby NodeRole = "standby"
	RoleSpare   NodeRole = "spare"
	RoleWorker  NodeRole = "worker"
	RoleUnknown NodeRole = "unknown"
)

type Node struct {
	Address string   `json:"address"`
	Arch    string   `json:"arch"`
	Role    NodeRole `json:"role"`
	// Leader reports whether this member currently holds the raft leadership.
	Leader bool `json:"leader"`
	// Reachability is the OBSERVED liveness of the node, from an actual bounded
	// dqlite RPC — not an inference from the node being listed. Membership is
	// not availability (issue #494).
	Reachability clustersvc.Reachability `json:"reachability"`
	LatencyMs    *int64                  `json:"latency_ms,omitempty"`
	// WorkersBusy is null when the database could not be read within the
	// enrichment budget — "not known" is not the same claim as "zero busy".
	WorkersBusy  *int `json:"workers_busy"`
	WorkersTotal int  `json:"workers_total"`
}

type Features struct {
	DatabaseConsoleEnabled     bool   `json:"database_console_enabled"`
	LogConsoleEnabled          bool   `json:"log_console_enabled"`
	ExternalURL                string `json:"external_url,omitempty"`
	AgentRemediationEnabled    bool   `json:"agent_remediation_enabled"`
	FreshnessEnabled           bool   `json:"freshness_enabled"`
	ContractEnforcementEnabled bool   `json:"contract_enforcement_enabled"`
	// DataAssertionsEnabled reports the data circuit breaker's master gate
	// (CAESIUM_DATA_ASSERTIONS_ENABLED). Off means no evaluator, no metrics
	// persistence and no hold routes, so the console hides the gated surfaces.
	DataAssertionsEnabled bool `json:"data_assertions_enabled"`
}

// nodeEnrichmentTimeout bounds the database work behind /v1/system/nodes.
//
// These queries route through dqlite, whose driver retries leader discovery
// until its CONTEXT is done (RetryLimit 0). Without a deadline they block for
// as long as the cluster has no leader — which is exactly the situation this
// endpoint exists to describe. A bounded enrichment degrades (worker counts go
// unknown) instead of hanging, so membership and liveness are still served.
const nodeEnrichmentTimeout = 900 * time.Millisecond

// nodeUsage is the database-derived part of the node list: which addresses have
// executed tasks, and how many tasks each is running now.
type nodeUsage struct {
	workers map[string]int
	// historical lists addresses seen in task claims that are not raft members.
	historical []string
}

type Service struct {
	ctx context.Context
	db  *gorm.DB
	// snapshot and enrich are fields so the bound can be proven with a
	// deliberately blocking query, without a database.
	snapshot func() clustersvc.View
	enrich   func(context.Context) (nodeUsage, error)
	timeout  time.Duration
}

func New(ctx context.Context) *Service {
	s := &Service{
		ctx:      ctx,
		db:       db.Connection(),
		snapshot: clustersvc.Snapshot,
		timeout:  nodeEnrichmentTimeout,
	}
	s.enrich = s.queryNodeUsage
	return s
}

func (s *Service) Nodes() ([]Node, error) {
	// Raft membership plus OBSERVED liveness, read FIRST and from an in-memory
	// snapshot. It never issues cluster RPCs on the request path, so it is
	// always available — including while the database work below cannot
	// complete because the cluster has no leader.
	view := s.snapshot()

	log.Debug("discovered dqlite nodes", "count", len(view.Members), "observed", view.Observed)

	type entry struct {
		role         NodeRole
		leader       bool
		reachability clustersvc.Reachability
		latencyMs    *int64
	}

	addrMap := make(map[string]entry)
	for _, m := range view.Members {
		addrMap[m.Address] = entry{
			role:         roleFromCluster(m.Role),
			leader:       m.Leader,
			reachability: m.Reachability,
			latencyMs:    m.LatencyMs,
		}
	}

	// Supplement with nodes from environment variables (fallback/seed). Their
	// liveness is unknown: they were never probed as raft members.
	for _, addr := range env.Variables().DatabaseNodes {
		addr = strings.TrimSpace(addr)
		if addr != "" {
			if _, ok := addrMap[addr]; !ok {
				addrMap[addr] = entry{role: RoleUnknown, reachability: clustersvc.Unknown}
			}
		}
	}
	if addr := env.Variables().NodeAddress; addr != "" {
		if _, ok := addrMap[addr]; !ok {
			addrMap[addr] = entry{role: RoleUnknown, reachability: clustersvc.Unknown}
		}
	}

	// Everything else needs the database. Bound it and degrade on timeout
	// rather than hanging: worker counts are a detail, membership and liveness
	// are the point of this endpoint.
	usage, usageKnown := s.nodeUsage()
	for _, addr := range usage.historical {
		if _, ok := addrMap[addr]; !ok {
			// Historical/distributed workers are not raft members, so their
			// liveness is likewise unknown.
			addrMap[addr] = entry{role: RoleWorker, reachability: clustersvc.Unknown}
		}
	}

	nodes := make([]Node, 0, len(addrMap))
	for addr, e := range addrMap {
		reachability := e.reachability
		if reachability == "" {
			reachability = clustersvc.Unknown
		}

		var busy *int
		if usageKnown {
			count := usage.workers[addr]
			busy = &count
		}

		nodes = append(nodes, Node{
			Address:      addr,
			Arch:         runtime.GOARCH,
			Role:         e.role,
			Leader:       e.leader,
			Reachability: reachability,
			LatencyMs:    e.latencyMs,
			WorkersBusy:  busy,
			WorkersTotal: env.Variables().WorkerPoolSize,
		})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Address < nodes[j].Address })
	return nodes, nil
}

// nodeUsage runs the database-backed enrichment under a deadline and reports
// whether it completed. The deadline is passed to the queries so the dqlite
// driver abandons leader discovery, and the select is the backstop that
// guarantees this returns even if a query ignores it.
func (s *Service) nodeUsage() (nodeUsage, bool) {
	timeout := s.timeout
	if timeout <= 0 {
		timeout = nodeEnrichmentTimeout
	}
	ctx, cancel := context.WithTimeout(s.ctx, timeout)
	defer cancel()

	type outcome struct {
		usage nodeUsage
		err   error
	}
	done := make(chan outcome, 1) // buffered: the goroutine never blocks
	go func() {
		usage, err := s.enrich(ctx)
		done <- outcome{usage: usage, err: err}
	}()

	select {
	case result := <-done:
		if result.err != nil {
			log.Warn("system nodes: usage enrichment failed", "error", result.err)
			return nodeUsage{}, false
		}
		return result.usage, true
	case <-ctx.Done():
		log.Warn("system nodes: usage enrichment timed out", "timeout", timeout)
		return nodeUsage{}, false
	}
}

// queryNodeUsage reads task claims in TWO queries rather than one per address:
// the per-address count used to be issued inside the node loop, so a large
// membership multiplied the blocking surface by the number of nodes.
func (s *Service) queryNodeUsage(ctx context.Context) (nodeUsage, error) {
	usage := nodeUsage{workers: map[string]int{}}

	var claimedBy []string
	if err := s.db.WithContext(ctx).Model(&models.TaskRun{}).
		Where("claimed_by != ''").
		Distinct("claimed_by").
		Pluck("claimed_by", &claimedBy).Error; err != nil {
		return nodeUsage{}, err
	}
	usage.historical = claimedBy

	var rows []struct {
		ClaimedBy string
		Busy      int
	}
	if err := s.db.WithContext(ctx).Model(&models.TaskRun{}).
		Select("claimed_by, COUNT(*) AS busy").
		Where("status = ? AND claimed_by != ''", "running").
		Group("claimed_by").
		Scan(&rows).Error; err != nil {
		return nodeUsage{}, err
	}
	for _, row := range rows {
		usage.workers[row.ClaimedBy] = row.Busy
	}
	return usage, nil
}

func roleFromCluster(role string) NodeRole {
	switch role {
	case clustersvc.RoleVoter:
		return RoleVoter
	case clustersvc.RoleStandby:
		return RoleStandby
	case clustersvc.RoleSpare:
		return RoleSpare
	default:
		return RoleUnknown
	}
}

func (s *Service) Features() (*Features, error) {
	v := env.Variables()
	return &Features{
		DatabaseConsoleEnabled:     v.DatabaseConsoleEnabled,
		LogConsoleEnabled:          v.LogConsoleEnabled,
		ExternalURL:                v.APIExternalURL,
		AgentRemediationEnabled:    v.AgentRemediationEnabled,
		FreshnessEnabled:           v.FreshnessEnabled,
		ContractEnforcementEnabled: contractsvc.Enabled(),
		DataAssertionsEnabled:      v.DataAssertionsEnabled,
	}, nil
}
