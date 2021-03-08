package capsule

import (
	"io"
	"time"
)

// Capsule defines the interface for interacting with
// an individual Capsule. A Capsule is analagous to a
// Docker container or a Kubernetes pod/deployment.
type Capsule interface {
	ID() string
	State() State
	Result() Result
	CreatedAt() time.Time
	StartedAt() time.Time
	StoppedAt() time.Time
}

// Engine defines the interface for interacting with
// a Capsule environment. A capsule.Engine is analagous
// to a Docker daemon or a Kubernetes master.
type Engine interface {
	Get(*EngineGetRequest) (Capsule, error)
	List(*EngineListRequest) ([]Capsule, error)
	Create(*EngineCreateRequest) (Capsule, error)
	Stop(*EngineStopRequest) error
	Logs(*EngineLogsRequest) (io.ReadCloser, error)
}

// EngineGetRequest defines the input parameters to
// an Engine.Get request.
type EngineGetRequest struct {
	ID string
}

// EngineListRequest defines the input parameters to
// an Engine.List request.
type EngineListRequest struct {
	Since  time.Time
	Before time.Time
}

// EngineCreateRequest defines the input parameters to
// an Engine.Create request.
type EngineCreateRequest struct {
	Name    string
	Image   string
	Command []string
}

// EngineStopRequest defines the input paramters to
// an Engine.Stop request.
type EngineStopRequest struct {
	ID      string
	Force   bool
	Timeout time.Duration
}

// EngineLogsRequest defines the input parameters to
// an Engine.Logs request.
type EngineLogsRequest struct {
	ID     string
	Stdout bool
	Stderr bool
	Since  time.Time
	Until  time.Time
}

// State defines the various states a Capsule can be in
// during its lifecycle while executing jobs.
type State string

const (
	// Created occurs immediately after engine.Create is
	// called successfully.
	Created State = "created"
	// Running occurs after a Capsule has been created,
	// and has begun executing its job. This is verified
	// by at least one subsequent State() call after
	// engine.Create is called.
	Running State = "running"
	// Stopping occurs immediately after engine.Stop is
	// called successfully.
	Stopping State = "stopping"
	// Stopped occurs after a capsule has been stopped,
	// and has returned at least one subsequent State()
	// call after engine.Stop is called.
	Stopped State = "stopped"
	// Deleting occurs immediately after engine.Delete
	// is called successfully.
	Deleting State = "deleting"
	// Deleted occurs after a capsule has been deleted,
	// and the Capsule no longer appears in the list
	// of Capsules returned by engine.List.
	Deleted State = "deleted"
	// Invalid occurs if a capsule.Engine returns an
	// unknown or unexpected state. If a Capsule reaches
	// an Invalid state, alerts will fire and it will be
	// deleted/retried.
	Invalid State = "invalid"
)

// Result defines the various end states that the Capsule
// terminates in, depending on where and why in during
// the lifecycle it was terminated.
type Result string

const (
	// Success result is returned if the Capsule was able
	// to successfully complete its Job with an exit
	// code of 0 returned by the underlying code.
	Success Result = "success"
	// Failure result is returned if the underlying job
	// has failed, but the Capsule itself behaved as
	// expected.
	Failure Result = "failure"
	// StartupFailure result is returned if the Capsule
	// was misconfigured and was unable to ever reach
	// a running state upon creation.
	StartupFailure Result = "startup_failure"
	// ResourceFailure result is returned if the Capsule
	// was unable to successfully complete its job due
	// to resource exhaustion. This could be an OOM kill
	// for example, or an eviction.
	ResourceFailure Result = "resource_failure"
	// Killed result is returned if the Capsule was
	// ungracefully terminated via a SIGKILL signal.
	Killed Result = "killed"
	// Terminated result is returned if the Capsule was
	// gracefully terminated via a SIGTERM signal.
	Terminated Result = "terminated"
	// Unknown result is returned if the capsule.Engine
	// returns an unexpected exit code. A Capsule that
	// results Unknown will be treated as a Failure.
	Unknown Result = "unknown"
)
