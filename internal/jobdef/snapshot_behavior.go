package jobdef

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
)

const secretRefPrefix = "secret://"

// stepBehaviorJSON returns the canonical JSON of every behavior-affecting field
// of a step that the snapshot descriptor does not already carry as a first-class
// field: env, nodeSelector, retries, retryDelay, retryBackoff, triggerRule,
// cache, inputSchema/outputSchema, volume mounts, workload identity, kueue,
// rateLimit, fanOut, datasets, engine, and so on. The step struct is marshalled
// as a whole so a field added to the schema later is covered automatically.
//
// Left out on purpose:
//   - name, image, command: first-class descriptor fields;
//   - next, dependsOn: the edge descriptors and the hash already cover them;
//   - ResolvedVolumeMounts and Kubernetes: derived runtime state, re-expressed
//     by volumeMounts and the workload-identity fields, never authoring input.
//
// The job-level declaration of each mounted volume (kind, pvc claim name and a
// digest of every per-engine source body; never the raw source) is
// recorded under "volumeSources", since the step carries only the mount.
//
// Env values never reach the descriptor in the clear (see redactEnv). Job-level
// settings (taskTimeout, runTimeout, sla, ...) are not step fields and are not
// covered. The output is deterministic: encoding/json sorts map keys and emits
// struct fields in declaration order.
func stepBehaviorJSON(step schema.Step, volumes []schema.Volume) (json.RawMessage, error) {
	step.Name = ""
	step.Image = ""
	step.Command = nil
	step.Next = nil
	step.DependsOn = nil
	step.ResolvedVolumeMounts = nil
	step.Kubernetes = nil
	step.Env = redactEnv(step.Env)

	raw, err := json.Marshal(step)
	if err != nil {
		return nil, err
	}
	// Step marshals name/image unconditionally; drop the now-empty first-class
	// keys so the behavior blob holds only behavior.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	delete(fields, "name")
	delete(fields, "image")
	if mounted := mountedVolumes(step, volumes); len(mounted) > 0 {
		raw, err := json.Marshal(mounted)
		if err != nil {
			return nil, err
		}
		fields["volumeSources"] = raw
	}
	for k, v := range fields {
		if string(v) == "null" { // cache is serialised without omitempty
			delete(fields, k)
		}
	}
	return json.Marshal(fields)
}

// volumeIdentity is the snapshot form of a mounted volume: its name, access
// mode and, per engine, a redacted source (see redactSource).
type volumeIdentity struct {
	Name       string                    `json:"name"`
	AccessMode string                    `json:"accessMode,omitempty"`
	Sources    map[string]sourceIdentity `json:"sources"`
}

// sourceIdentity keeps only the non-sensitive identity of a volume source in
// clear: its kind and, for a pvc, the claim name. Everything else lives behind
// Digest.
type sourceIdentity struct {
	Kind   string `json:"kind"`
	Claim  string `json:"claim,omitempty"`
	Digest string `json:"digest"`
}

// mountedVolumes returns the redacted job-level declaration of each volume the
// step mounts, sorted by name. The step only carries the mount (volume name +
// path); the source lives on the job, so without this a PVC claim or host path
// edit would dedup the snapshot. A raw source is never stored: a Kubernetes
// volumeSource accepts arbitrary config (flexVolume.options, csi
// volumeAttributes) that can hold credentials, so each source body is replaced
// by a digest of its canonical JSON.
func mountedVolumes(step schema.Step, volumes []schema.Volume) []volumeIdentity {
	if len(step.VolumeMounts) == 0 || len(volumes) == 0 {
		return nil
	}
	wanted := make(map[string]struct{}, len(step.VolumeMounts))
	for _, m := range step.VolumeMounts {
		wanted[m.Volume] = struct{}{}
	}
	out := make([]volumeIdentity, 0, len(wanted))
	for _, v := range volumes {
		if _, ok := wanted[v.Name]; !ok {
			continue
		}
		id := volumeIdentity{Name: v.Name, AccessMode: v.AccessMode, Sources: map[string]sourceIdentity{}}
		if v.Source != nil {
			id.Sources["default"] = redactSource(*v.Source)
		}
		for engine, src := range v.Sources {
			id.Sources[engine] = redactSource(src)
		}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func redactSource(src schema.VolumeSource) sourceIdentity {
	id := sourceIdentity{}
	switch {
	case strings.TrimSpace(src.PVC) != "":
		id.Kind, id.Claim = "pvc", src.PVC
	case src.ClaimTemplate != nil:
		id.Kind = "claimTemplate"
	case src.VolumeSource != nil:
		id.Kind = "volumeSource"
	case strings.TrimSpace(src.Bind) != "":
		id.Kind = "bind"
	case strings.TrimSpace(src.Volume) != "":
		id.Kind = "volume"
	case src.Tmpfs != nil:
		id.Kind = "tmpfs"
	}
	raw, _ := json.Marshal(src) // map keys are sorted: canonical
	sum := sha256.Sum256(raw)
	id.Digest = "sha256:" + hex.EncodeToString(sum[:])[:16]
	return id
}

// redactEnv keeps change-detection without ever storing a value that may be a
// secret: a secret:// reference is a pointer, not a value, and is kept
// verbatim; every other value is replaced by a truncated SHA-256 digest, so a
// literal credential pasted into a manifest does not leak through blame or the
// topology history API while an edit to it still changes the descriptor.
func redactEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		if strings.HasPrefix(strings.TrimSpace(v), secretRefPrefix) {
			out[k] = v
			continue
		}
		sum := sha256.Sum256([]byte(v))
		out[k] = "sha256:" + hex.EncodeToString(sum[:])[:16]
	}
	return out
}
