package podman

import (
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/containers/podman/v4/libpod/define"
)

type Atom struct {
	atom.Atom
	metadata *define.InspectContainerData
}

func (a *Atom) ID() string {
	return a.metadata.ID
}

func (a *Atom) State() atom.State {
	if state, ok := stateMap[a.metadata.State.Status]; ok {
		return state
	}
	return atom.Invalid
}

func (a *Atom) Result() atom.Result {
	if result, ok := resultMap[int(a.metadata.State.ExitCode)]; ok {
		return result
	}
	return atom.Unknown
}

func (a *Atom) CreatedAt() time.Time {
	return a.metadata.Created
}

func (a *Atom) StartedAt() time.Time {
	return a.metadata.State.StartedAt
}

func (a *Atom) StoppedAt() time.Time {
	return a.metadata.State.FinishedAt
}
