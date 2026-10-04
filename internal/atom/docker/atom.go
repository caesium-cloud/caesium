package docker

import (
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/docker/docker/api/types/container"
)

// Atom defines the interface for treating
// Docker containers as Caesium Atoms.
type Atom struct {
	atom.Atom
	metadata container.InspectResponse
	// oomUnresolved marks a wait outcome whose exit-137 OOM evidence could not
	// converge; its false OOMKilled flag is not a verdict.
	oomUnresolved bool
}

// ID returns the ID of the Atom. This ID is identical
// to the Docker ID assigned by the Docker daemon.
func (c *Atom) ID() string {
	return c.metadata.ID
}

// State returns the state of the Atom. This function
// maps Docker container states to Caesium Atom states.
func (c *Atom) State() atom.State {
	return atom.ContainerState(c.metadata.State.Status)
}

// Result returns the result of the Atom. This function
// maps Docker container exit codes to Caesium Atom results.
func (c *Atom) Result() atom.Result {
	if env.Variables().ResourceStatsEnabled && c.ResourceOutcome().OOMKilled {
		return atom.ResourceFailure
	}
	return atom.ResultForExitCode(c.metadata.State.ExitCode)
}

// ExitCode returns the raw Docker container exit code, preserved for the
// incident classifier before Result() folds it into a coarse Result.
func (c *Atom) ExitCode() *int {
	if c.metadata.State == nil {
		return nil
	}
	code := c.metadata.State.ExitCode
	return &code
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

func (c *Atom) ResourceOutcome() atom.ResourceOutcome {
	if c.metadata.ContainerJSONBase == nil || c.metadata.State == nil {
		return atom.ResourceOutcome{}
	}
	out := atom.ResourceOutcome{OOMKnown: !c.oomUnresolved || c.metadata.State.OOMKilled, OOMKilled: c.metadata.State.OOMKilled}
	if c.metadata.HostConfig != nil && c.metadata.HostConfig.Memory > 0 {
		value := c.metadata.HostConfig.Memory
		out.MemoryLimitBytes = &value
	}
	return out
}
