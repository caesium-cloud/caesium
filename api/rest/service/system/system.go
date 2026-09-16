package system

import (
	"context"
	"runtime"
	"sort"
	"strings"

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
	WorkersBusy  int                     `json:"workers_busy"`
	WorkersTotal int                     `json:"workers_total"`
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

type Service struct {
	ctx context.Context
	db  *gorm.DB
}

func New(ctx context.Context) *Service {
	return &Service{ctx: ctx, db: db.Connection()}
}

func (s *Service) Nodes() ([]Node, error) {
	// Raft membership plus OBSERVED liveness. The view is refreshed in the
	// background, so this never issues cluster RPCs on the request path.
	view := clustersvc.Snapshot()

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

	// Discover nodes that have worked (historical/distributed workers). These
	// are not raft members, so their liveness is likewise unknown.
	var claimedBy []string
	if err := s.db.WithContext(s.ctx).Model(&models.TaskRun{}).
		Where("claimed_by != ''").
		Distinct("claimed_by").
		Pluck("claimed_by", &claimedBy).Error; err == nil {
		for _, addr := range claimedBy {
			if _, ok := addrMap[addr]; !ok {
				addrMap[addr] = entry{role: RoleWorker, reachability: clustersvc.Unknown}
			}
		}
	}

	nodes := make([]Node, 0, len(addrMap))
	for addr, e := range addrMap {
		var busy int64
		s.db.WithContext(s.ctx).Model(&models.TaskRun{}).
			Where("status = ? AND claimed_by = ?", "running", addr).
			Count(&busy)

		reachability := e.reachability
		if reachability == "" {
			reachability = clustersvc.Unknown
		}

		nodes = append(nodes, Node{
			Address:      addr,
			Arch:         runtime.GOARCH,
			Role:         e.role,
			Leader:       e.leader,
			Reachability: reachability,
			LatencyMs:    e.latencyMs,
			WorkersBusy:  int(busy),
			WorkersTotal: env.Variables().WorkerPoolSize,
		})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Address < nodes[j].Address })
	return nodes, nil
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
