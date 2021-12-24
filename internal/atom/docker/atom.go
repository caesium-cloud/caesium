package docker

import (
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/docker/docker/api/types"
)

// Atom defines the interface for treating
// Docker containers as Caesium Atoms.
type Atom struct {
	atom.Atom
	metadata types.ContainerJSON
}

// ID returns the ID of the Atom. This ID is identical
// to the Docker ID assigned by the Docker daemon.
func (c *Atom) ID() string {
	return c.metadata.ID
}

// State returns the state of the Atom. This function
// maps Docker container states to Caesium Atom states.
func (c *Atom) State() atom.State {
	if state, ok := stateMap[c.metadata.State.Status]; ok {
		return state
	}
	return atom.Invalid
}

// Result returns the result of the Atom. This function
// maps Docker container exit codes to Caesium Atom results.
func (c *Atom) Result() atom.Result {
	if result, ok := resultMap[c.metadata.State.ExitCode]; ok {
		return result
	}
	return atom.Unknown
}

// CreatedAt returns the UTC time the Atom was created.
func (c *Atom) CreatedAt() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, c.metadata.Created)
	return t
}

// StartedAt returns the UTC time the Atom was started.
func (c *Atom) StartedAt() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, c.metadata.State.StartedAt)
	return t
}

// StoppedAt returns the UTC time the Atom was stopped.
func (c *Atom) StoppedAt() time.Time {
	t, _ := time.Parse(time.RFC3339, c.metadata.State.FinishedAt)
	return t
}
