package kubernetes

import (
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/models"
	v1 "k8s.io/api/core/v1"
)

// Atom defines the interface for treating
// Kubernetes pods as Caesium Atoms.
type Atom struct {
	atom.Atom
	metadata *v1.Pod
}

func New(a *models.Atom) (atom.Atom, error) {
	return nil, nil
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
	if term := terminatedState(c.metadata); term != nil {
		if result, ok := resultMap[term.ExitCode]; ok {
			return result
		}
		return atom.Unknown
	}
	switch c.metadata.Status.Phase {
	case v1.PodSucceeded:
		return atom.Success
	case v1.PodFailed:
		return atom.Failure
	default:
		return atom.Unknown
	}
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
	if c.metadata.DeletionTimestamp != nil && !c.metadata.DeletionTimestamp.Time.IsZero() {
		return c.metadata.DeletionTimestamp.Time
	}
	if term := terminatedState(c.metadata); term != nil {
		if !term.FinishedAt.IsZero() {
			return term.FinishedAt.Time
		}
		if !term.StartedAt.IsZero() {
			return term.StartedAt.Time
		}
	}
	switch c.metadata.Status.Phase {
	case v1.PodSucceeded, v1.PodFailed:
		if c.metadata.Status.StartTime != nil {
			return c.metadata.Status.StartTime.Time
		}
		return c.metadata.CreationTimestamp.Time
	default:
		return time.Time{}
	}
}

func terminatedState(pod *v1.Pod) *v1.ContainerStateTerminated {
	if pod == nil {
		return nil
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Terminated != nil {
			return status.State.Terminated
		}
	}
	return nil
}
