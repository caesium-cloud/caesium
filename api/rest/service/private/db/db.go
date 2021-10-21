package db

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/db"
	"github.com/caesium-cloud/caesium/db/command"
	"github.com/caesium-cloud/caesium/db/store"
	"github.com/caesium-cloud/caesium/pkg/log"
)

type Database interface {
	WithStore(*store.Store) Database
	Query(*QueryRequest) (*QueryResponse, error)
	Execute(*ExecuteRequest) (*ExecuteResponse, error)
	Backup(*BackupRequest) error
	Load(*LoadRequest) (*LoadResponse, error)
}

type dbService struct {
	s *store.Store
}

func Service() Database {
	return &dbService{
		s: store.GlobalStore(),
	}
}

func (d *dbService) WithStore(s *store.Store) Database {
	d.s = s
	return d
}

type Statement struct {
	Sql        string        `json:"sql"`
	Parameters []interface{} `json:"parameters"`
}

func (s *Statement) CommandParameters() []*command.Parameter {
	params := make([]*command.Parameter, len(s.Parameters))

	for i, p := range s.Parameters {
		switch v := p.(type) {
		case int64:
			params[i] = &command.Parameter{Value: &command.Parameter_I{I: v}}
		case float64:
			params[i] = &command.Parameter{Value: &command.Parameter_D{D: v}}
		case bool:
			params[i] = &command.Parameter{Value: &command.Parameter_B{B: v}}
		case []byte:
			params[i] = &command.Parameter{Value: &command.Parameter_Y{Y: v}}
		case time.Time:
			params[i] = &command.Parameter{
				Value: &command.Parameter_S{
					S: v.Format(time.RFC3339),
				},
			}
		default:
			params[i] = &command.Parameter{
				Value: &command.Parameter_S{
					S: fmt.Sprintf("%v", v),
				},
			}
		}
	}

	return params
}

type QueryRequest struct {
	Transaction bool                       `json:"transaction"`
	Timings     bool                       `json:"timings"`
	Level       command.QueryRequest_Level `json:"level"`
	Freshness   time.Duration              `json:"freshness"`
	Statements  []*Statement               `json:"statements"`
}

type QueryResponse struct {
	Results []*db.Rows    `json:"results,omitempty"`
	Time    time.Duration `json:"time,omitempty"`
}

func (d *dbService) Query(req *QueryRequest) (*QueryResponse, error) {
	statements := make([]*command.Statement, len(req.Statements))

	for i, s := range req.Statements {
		if !strings.HasSuffix(s.Sql, ";") {
			s.Sql += ";"
		}

		log.Debug("db query", "statement", s.Sql)

		statements[i] = &command.Statement{
			Sql:        s.Sql,
			Parameters: s.CommandParameters(),
		}
	}

	cmd := &command.QueryRequest{
		Request: &command.Request{
			Transaction: req.Transaction,
			Statements:  statements,
		},
		Timings:   req.Timings,
		Level:     req.Level,
		Freshness: req.Freshness.Nanoseconds(),
	}

	start := time.Now()

	results, err := d.s.Query(cmd)
	if err != nil {
		return nil, err
	}

	resp := &QueryResponse{Results: results}
	if req.Timings {
		resp.Time = time.Since(start)
	}

	log.Debug("db query", "results", resp.Results[0])

	return resp, nil
}

type ExecuteRequest struct {
	Transaction bool         `json:"transaction"`
	Timings     bool         `json:"timings"`
	Statements  []*Statement `json:"statements"`
	AllOrNone   bool         `json:"all_or_none"`
}

type ExecuteResponse struct {
	Results []*db.Result  `json:"results,omitempty"`
	Time    time.Duration `json:"time,omitempty"`
}

func (d *dbService) Execute(req *ExecuteRequest) (*ExecuteResponse, error) {
	statements := make([]*command.Statement, len(req.Statements))

	for i, s := range req.Statements {
		if !strings.HasSuffix(s.Sql, ";") {
			s.Sql += ";"
		}

		log.Debug("db execution", "statement", s)

		statements[i] = &command.Statement{
			Sql:        s.Sql,
			Parameters: s.CommandParameters(),
		}
	}

	cmd := &command.ExecuteRequest{
		Request: &command.Request{
			Transaction: req.Transaction,
			Statements:  statements,
		},
		Timings: req.Timings,
	}

	var (
		start   = time.Now()
		results []*db.Result
		err     error
	)

	if req.AllOrNone {
		results, err = d.s.ExecuteOrAbort(cmd)
	} else {
		results, err = d.s.Execute(cmd)
	}

	if err != nil {
		return nil, err
	}

	resp := &ExecuteResponse{Results: results}
	if req.Timings {
		resp.Time = time.Since(start)
	}

	log.Debug("db execution", "results", resp.Results[0])

	return resp, nil
}

type BackupRequest struct {
	LeaderOnly bool               `json:"leader_only"`
	Format     store.BackupFormat `json:"format"`
	Writer     io.Writer          `json:"-"`
}

func (d *dbService) Backup(req *BackupRequest) error {
	return d.s.Backup(req.LeaderOnly, req.Format, req.Writer)
}

type LoadRequest struct {
	Timings bool     `json:"timings"`
	Queries []string `json:"queries"`
}

type LoadResponse struct {
	Results interface{}   `json:"results,omitempty"`
	Time    time.Duration `json:"time,omitempty"`
}

func (d *dbService) Load(req *LoadRequest) (*LoadResponse, error) {
	statements := make([]*command.Statement, len(req.Queries))

	for i, q := range req.Queries {
		statements[i] = &command.Statement{Sql: q}
	}

	cmd := &command.ExecuteRequest{
		Request: &command.Request{
			Statements:  statements,
			Transaction: false,
		},
		Timings: req.Timings,
	}

	start := time.Now()

	results, err := d.s.ExecuteOrAbort(cmd)
	if err != nil {
		return nil, err
	}

	resp := &LoadResponse{Results: results}
	if req.Timings {
		resp.Time = time.Since(start)
	}

	return resp, nil
}
