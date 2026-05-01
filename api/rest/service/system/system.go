package system

import (
	"context"
	"fmt"
	"runtime"
	"strings"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/caesium-cloud/caesium/pkg/dqlite"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/canonical/go-dqlite/v3/client"
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
	Address      string   `json:"address"`
	Arch         string   `json:"arch"`
	Role         NodeRole `json:"role"`
	WorkersBusy  int      `json:"workers_busy"`
	WorkersTotal int      `json:"workers_total"`
}

type Features struct {
	DatabaseConsoleEnabled bool   `json:"database_console_enabled"`
	LogConsoleEnabled      bool   `json:"log_console_enabled"`
	ExternalURL            string `json:"external_url,omitempty"`
}

type Service struct {
	ctx context.Context
	db  *gorm.DB
}

func New(ctx context.Context) *Service {
	return &Service{ctx: ctx, db: db.Connection()}
}

func (s *Service) Nodes() ([]Node, error) {
	// Start with authoritative Raft membership from dqlite.
	cluster, err := dqlite.ClusterNodes(s.ctx)
	if err != nil {
		log.Warn("failed to fetch dqlite cluster nodes", "error", err)
	}

	log.Debug("discovered dqlite nodes", "count", len(cluster))

	addrMap := make(map[string]NodeRole) // address -> role
	for _, n := range cluster {
		var role NodeRole
		switch n.Role {
		case client.Voter:
			role = RoleVoter
		case 1: // Standby
			role = RoleStandby
		case 2: // Spare
			role = RoleSpare
		default:
			role = NodeRole(fmt.Sprintf("unknown(%d)", n.Role))
		}
		addrMap[n.Address] = role
	}

	// Supplement with nodes from environment variables (fallback/seed).
	for _, addr := range env.Variables().DatabaseNodes {
		addr = strings.TrimSpace(addr)
		if addr != "" {
			if _, ok := addrMap[addr]; !ok {
				addrMap[addr] = RoleUnknown
			}
		}
	}
	if addr := env.Variables().NodeAddress; addr != "" {
		if _, ok := addrMap[addr]; !ok {
			addrMap[addr] = RoleVoter
		}
	}

	// Discover nodes that have worked (historical/distributed workers).
	var claimedBy []string
	if err := s.db.WithContext(s.ctx).Model(&models.TaskRun{}).
		Where("claimed_by != ''").
		Distinct("claimed_by").
		Pluck("claimed_by", &claimedBy).Error; err == nil {
		for _, addr := range claimedBy {
			if _, ok := addrMap[addr]; !ok {
				addrMap[addr] = RoleWorker
			}
		}
	}

	nodes := make([]Node, 0, len(addrMap))
	for addr, role := range addrMap {
		var busy int64
		s.db.WithContext(s.ctx).Model(&models.TaskRun{}).
			Where("status = ? AND claimed_by = ?", "running", addr).
			Count(&busy)

		nodes = append(nodes, Node{
			Address:      addr,
			Arch:         runtime.GOARCH,
			Role:         role,
			WorkersBusy:  int(busy),
			WorkersTotal: env.Variables().WorkerPoolSize,
		})
	}
	return nodes, nil
}

func (s *Service) Features() (*Features, error) {
	v := env.Variables()
	return &Features{
		DatabaseConsoleEnabled: v.DatabaseConsoleEnabled,
		LogConsoleEnabled:      v.LogConsoleEnabled,
		ExternalURL:            v.APIExternalURL,
	}, nil
}
