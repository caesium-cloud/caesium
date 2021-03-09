package docker

import (
	"time"

	"github.com/caesium-dev/caesium/internal/capsule"
	"github.com/docker/docker/api/types"
)

// Capsule defines the interface for treating
// Docker containers as Caesium Capsules.
type Capsule struct {
	capsule.Capsule
	metadata types.ContainerJSON
}

// ID returns the ID of the Capsule. This ID is identical
// to the Docker ID assigned by the Docker daemon.
func (c *Capsule) ID() string {
	return c.metadata.ID
}

// State returns the state of the Capsule. This function
// maps Docker container states to Caesium Capsule states.
func (c *Capsule) State() capsule.State {
	if state, ok := stateMap[c.metadata.State.Status]; ok {
		return state
	}
	return capsule.Invalid
}

// Result returns the result of the Capsule. This function
// maps Docker container exit codes to Caesium Capsule results.
func (c *Capsule) Result() capsule.Result {
	if result, ok := resultMap[c.metadata.State.ExitCode]; ok {
		return result
	}
	return capsule.Unknown
}

// CreatedAt returns the UTC time the Capsule was created.
func (c *Capsule) CreatedAt() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, c.metadata.Created)
	return t
}

// StartedAt returns the UTC time the Capsule was started.
func (c *Capsule) StartedAt() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, c.metadata.State.StartedAt)
	return t
}

// StoppedAt returns the UTC time the Capsule was stopped.
func (c *Capsule) StoppedAt() time.Time {
	t, _ := time.Parse(time.RFC3339, c.metadata.State.FinishedAt)
	return t
}
