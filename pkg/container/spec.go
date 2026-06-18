package container

// MountType enumerates supported mount driver types.
type MountType string

const (
	// MountTypeBind represents a bind mount from the host filesystem.
	MountTypeBind MountType = "bind"
	// MountTypeVolume represents a Docker/Podman named volume.
	MountTypeVolume MountType = "volume"
	// MountTypeTmpfs represents a tmpfs mount.
	MountTypeTmpfs MountType = "tmpfs"
)

// Mount describes a filesystem mount to inject into a container.
type Mount struct {
	Type     MountType `json:"type" yaml:"type"`
	Source   string    `json:"source" yaml:"source"`
	Target   string    `json:"target" yaml:"target"`
	ReadOnly bool      `json:"readOnly,omitempty" yaml:"readOnly,omitempty"`
}

// VolumeMountType enumerates resolved volume sources. These are produced from
// job-level volumes and consumed by engine adapters.
type VolumeMountType string

const (
	VolumeMountTypeBind          VolumeMountType = "bind"
	VolumeMountTypeVolume        VolumeMountType = "volume"
	VolumeMountTypeTmpfs         VolumeMountType = "tmpfs"
	VolumeMountTypePVC           VolumeMountType = "pvc"
	VolumeMountTypeClaimTemplate VolumeMountType = "claimTemplate"
	VolumeMountTypeVolumeSource  VolumeMountType = "volumeSource"
)

// TmpfsOptions captures the portable tmpfs options Caesium understands.
type TmpfsOptions struct {
	SizeBytes int64 `json:"sizeBytes,omitempty" yaml:"sizeBytes,omitempty"`
	Mode      *int  `json:"mode,omitempty" yaml:"mode,omitempty"`
}

// KubernetesClaimTemplate captures the fields Caesium needs to build an inline
// ephemeral Kubernetes PVC claim template.
type KubernetesClaimTemplate struct {
	StorageClass string            `json:"storageClass,omitempty" yaml:"storageClass,omitempty"`
	Size         string            `json:"size,omitempty" yaml:"size,omitempty"`
	AccessMode   string            `json:"accessMode,omitempty" yaml:"accessMode,omitempty"`
	Labels       map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty" yaml:"annotations,omitempty"`
}

// VolumeMount describes a resolved named volume mount in a runtime-neutral
// form. Engines map the type-specific fields into native Docker, Podman, or
// Kubernetes mount structures.
type VolumeMount struct {
	Name          string                   `json:"name,omitempty" yaml:"name,omitempty"`
	Type          VolumeMountType          `json:"type" yaml:"type"`
	Source        string                   `json:"source,omitempty" yaml:"source,omitempty"`
	Target        string                   `json:"target" yaml:"target"`
	ReadOnly      bool                     `json:"readOnly,omitempty" yaml:"readOnly,omitempty"`
	SubPath       string                   `json:"subPath,omitempty" yaml:"subPath,omitempty"`
	Tmpfs         *TmpfsOptions            `json:"tmpfs,omitempty" yaml:"tmpfs,omitempty"`
	ClaimTemplate *KubernetesClaimTemplate `json:"claimTemplate,omitempty" yaml:"claimTemplate,omitempty"`
	VolumeSource  map[string]any           `json:"volumeSource,omitempty" yaml:"volumeSource,omitempty"`
}

// KubernetesSpec captures Kubernetes-only pod settings that can be derived from
// job definitions without forcing the public container package to depend on
// Kubernetes API types.
type KubernetesSpec struct {
	ServiceAccountName           string            `json:"serviceAccountName,omitempty" yaml:"serviceAccountName,omitempty"`
	PodAnnotations               map[string]string `json:"podAnnotations,omitempty" yaml:"podAnnotations,omitempty"`
	AutomountServiceAccountToken *bool             `json:"automountServiceAccountToken,omitempty" yaml:"automountServiceAccountToken,omitempty"`
}

// Spec captures shared container runtime knobs regardless of engine.
type Spec struct {
	Env                  map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	WorkDir              string            `json:"workdir,omitempty" yaml:"workdir,omitempty"`
	Mounts               []Mount           `json:"mounts,omitempty" yaml:"mounts,omitempty"`
	ResolvedVolumeMounts []VolumeMount     `json:"resolvedVolumeMounts,omitempty" yaml:"-"`
	Kubernetes           *KubernetesSpec   `json:"kubernetes,omitempty" yaml:"-"`
}

// HasEnv reports whether any environment variables are defined.
func (s Spec) HasEnv() bool {
	return len(s.Env) > 0
}

// HasMounts reports whether any mounts are defined.
func (s Spec) HasMounts() bool {
	return len(s.Mounts) > 0 || len(s.ResolvedVolumeMounts) > 0
}
