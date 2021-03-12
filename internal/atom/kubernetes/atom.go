package kubernetes

import (
	"time"

	"github.com/caesium-dev/caesium/internal/atom"
	v1 "k8s.io/api/core/v1"
)

// Atom defines the interface for treating
// Kubernetes pods as Caesium Atoms.
type Atom struct {
	atom.Atom
	metadata *v1.Pod
}

// ID returns the ID of the Atom. This ID is
// identical to the Kubernetes pod name.
func (c *Atom) ID() string {
	return c.metadata.Name
}

// State returns the state of the Atom. This function
// maps pod phases to Caesium Atom states.
func (c *Atom) State() atom.State {
	if state, ok := stateMap[c.metadata.Status.Phase]; ok {
		return state
	}
	return atom.Invalid
}

// Result returns the result of the Atom. This function
// maps pod container exit codes to Caesium Atom results.
func (c *Atom) Result() atom.Result {
	container := c.metadata.Status.ContainerStatuses[0]

	if result, ok := resultMap[container.State.Terminated.ExitCode]; ok {
		return result
	}

	return atom.Unknown
}

// CreatedAt returns the UTC time the Atom was created.
func (c *Atom) CreatedAt() time.Time {
	return c.metadata.CreationTimestamp.Time
}

// StartedAt returns the UTC time the Atom was started.
func (c *Atom) StartedAt() time.Time {
	if c.metadata.Status.StartTime == nil {
		return time.Time{}
	}
	return c.metadata.Status.StartTime.Time
}

// StoppedAt returns the UTC time the Atom was stopped.
func (c *Atom) StoppedAt() time.Time {
	return c.metadata.DeletionTimestamp.Time
}
