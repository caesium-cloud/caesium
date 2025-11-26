package container

// MountType enumerates supported mount driver types.
type MountType string

const (
	// MountTypeBind represents a bind mount from the host filesystem.
	MountTypeBind MountType = "bind"
)

// Mount describes a filesystem mount to inject into a container.
type Mount struct {
	Type     MountType `json:"type" yaml:"type"`
	Source   string    `json:"source" yaml:"source"`
	Target   string    `json:"target" yaml:"target"`
	ReadOnly bool      `json:"readOnly,omitempty" yaml:"readOnly,omitempty"`
}

// Spec captures shared container runtime knobs regardless of engine.
type Spec struct {
	Env     map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	WorkDir string            `json:"workdir,omitempty" yaml:"workdir,omitempty"`
	Mounts  []Mount           `json:"mounts,omitempty" yaml:"mounts,omitempty"`
}

// HasEnv reports whether any environment variables are defined.
func (s Spec) HasEnv() bool {
	return len(s.Env) > 0
}

// HasMounts reports whether any mounts are defined.
func (s Spec) HasMounts() bool {
	return len(s.Mounts) > 0
}
