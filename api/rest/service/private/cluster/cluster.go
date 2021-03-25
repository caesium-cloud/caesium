package cluster

import (
	"fmt"
	"net/http"
	"runtime"
	"time"

	"github.com/caesium-cloud/caesium/db/store"
)

var startTime time.Time

func init() {
	startTime = time.Now()
}

type Cluster interface {
	Join(*JoinRequest) error
	Remove(*RemoveRequest) error
	Status() (*StatusResponse, error)
	LeaderAPIAddr() string
	LeaderAPIProto() string
	FormRedirect(*http.Request, string, string) string
}

type clusterService struct {
	s *store.Store
}

func Service() Cluster {
	return &clusterService{
		s: store.GlobalStore(),
	}
}

type JoinRequest struct {
	ID       string            `json:"id"`
	Address  string            `json:"address"`
	Voter    bool              `json:"voter"`
	Metadata map[string]string `json:"metadata"`
}

func (c *clusterService) Join(req *JoinRequest) error {
	return c.s.Join(
		req.ID,
		req.Address,
		req.Voter,
		req.Metadata,
	)
}

type RemoveRequest struct {
	ID string `json:"id"`
}

func (c *clusterService) Remove(req *RemoveRequest) error {
	return c.s.Remove(req.ID)
}

type StatusResponse struct {
	Runtime        *RuntimeStatus         `json:"runtime"`
	HTTP           *HTTPStatus            `json:"http"`
	Node           *NodeStatus            `json:"node"`
	Store          map[string]interface{} `json:"store"`
	LastBackupTime time.Time              // TODO: sort out
	Build          map[string]interface{} // TODO: sort out
}

type RuntimeStatus struct {
	GOARCH       string `json:"GOARCH"`
	GOOS         string `json:"GOOS"`
	GOMAXPROCS   int    `json:"GOMAXPROCS"`
	NumCPU       int    `json:"num_cpu"`
	NumGoRoutine int    `json:"num_goroutine"`
	Version      string `json:"version"`
}

type HTTPStatus struct {
	Address       string `json:"address"`
	Authenticated bool   `json:"authenticated"`
	Redirect      string `json:"redirect"`
}

type NodeStatus struct {
	StarTime time.Time     `json:"start_time"`
	Uptime   time.Duration `json:"uptime"`
}

func (c *clusterService) Status() (*StatusResponse, error) {
	results, err := c.s.Stats()
	if err != nil {
		return nil, err
	}

	return &StatusResponse{
		Runtime: &RuntimeStatus{
			GOARCH:       runtime.GOARCH,
			GOOS:         runtime.GOOS,
			GOMAXPROCS:   runtime.GOMAXPROCS(0),
			NumCPU:       runtime.NumCPU(),
			NumGoRoutine: runtime.NumGoroutine(),
			Version:      runtime.Version(),
		},
		HTTP: &HTTPStatus{
			Address:       c.s.Addr(),
			Authenticated: false, // TODO: see what we need to do here
			Redirect:      c.LeaderAPIAddr(),
		},
		Node: &NodeStatus{
			StarTime: startTime,
			Uptime:   time.Since(startTime),
		},
		Store: results,
	}, nil
}

func (c *clusterService) LeaderAPIAddr() string {
	id, err := c.s.LeaderID()
	if err != nil {
		return ""
	}
	return c.s.Metadata(id, "api_addr")
}

func (c *clusterService) LeaderAPIProto() string {
	id, err := c.s.LeaderID()
	if err != nil {
		return "http"
	}

	proto := c.s.Metadata(id, "api_proto")
	if proto == "" {
		return "http"
	}

	return proto
}

func (c *clusterService) FormRedirect(r *http.Request, protocol, host string) string {
	raw := r.URL.RawQuery
	if raw != "" {
		raw = fmt.Sprintf("?%s", raw)
	}
	return fmt.Sprintf("%s://%s%s%s", protocol, host, r.URL.Path, raw)
}
