package system

import (
	"context"
	"runtime"
	"strings"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/caesium-cloud/caesium/pkg/env"
	"gorm.io/gorm"
)

type Node struct {
	Address      string `json:"address"`
	Arch         string `json:"arch"`
	WorkersBusy  int    `json:"workers_busy"`
	WorkersTotal int    `json:"workers_total"`
}

type Features struct {
	DatabaseConsoleEnabled bool `json:"database_console_enabled"`
	LogConsoleEnabled      bool `json:"log_console_enabled"`
	UIRefreshV2System      bool `json:"ui_refresh_v2_system"`
}

type Service struct {
	ctx context.Context
	db  *gorm.DB
}

func New(ctx context.Context) *Service {
	return &Service{ctx: ctx, db: db.Connection()}
}

func (s *Service) Nodes() ([]Node, error) {
	addrs := env.Variables().DatabaseNodes
	if len(addrs) == 0 && env.Variables().NodeAddress != "" {
		addrs = []string{env.Variables().NodeAddress}
	}

	if len(addrs) == 0 {
		addrs = []string{"127.0.0.1:8080"}
	}

	nodes := make([]Node, 0, len(addrs))
	for _, addr := range addrs {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}

		var busy int64
		s.db.WithContext(s.ctx).Model(&models.TaskRun{}).Where("status = ? AND claimed_by = ?", "running", addr).Count(&busy)

		nodes = append(nodes, Node{
			Address:      addr,
			Arch:         runtime.GOARCH,
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
		UIRefreshV2System:      true, // This enables the V2 System UI in the frontend
	}, nil
}
