package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"sort"
	"strings"

	"github.com/caesium-cloud/caesium/pkg/container"
)

// HashInput contains all fields that contribute to a task's identity hash.
type HashInput struct {
	JobAlias           string
	TaskName           string
	Image              string
	Command            []string
	Env                map[string]string
	WorkDir            string
	Mounts             []container.Mount
	PredecessorHashes  []string
	PredecessorOutputs map[string]map[string]string
	RunParams          map[string]string
	CacheVersion       int
}

// Compute returns the SHA-256 hex digest of the canonicalized input.
func (h HashInput) Compute() string {
	digest := sha256.New()
	// Write each field in deterministic order
	w(digest, "job_alias:%s\n", h.JobAlias)
	w(digest, "task_name:%s\n", h.TaskName)
	w(digest, "image:%s\n", h.Image)
	w(digest, "command:%s\n", strings.Join(h.Command, "\x00"))

	// Sorted env vars
	envKeys := make([]string, 0, len(h.Env))
	for k := range h.Env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	for _, k := range envKeys {
		w(digest, "env:%s=%s\n", k, h.Env[k])
	}

	w(digest, "workdir:%s\n", h.WorkDir)

	// Sorted mounts (serialize each mount deterministically)
	mountStrs := make([]string, 0, len(h.Mounts))
	for _, m := range h.Mounts {
		mountStrs = append(mountStrs, fmt.Sprintf("%s:%s:%s:%t", m.Source, m.Target, m.Type, m.ReadOnly))
	}
	sort.Strings(mountStrs)
	for _, m := range mountStrs {
		w(digest, "mount:%s\n", m)
	}

	// Sorted predecessor hashes
	predHashes := make([]string, len(h.PredecessorHashes))
	copy(predHashes, h.PredecessorHashes)
	sort.Strings(predHashes)
	for _, ph := range predHashes {
		w(digest, "pred_hash:%s\n", ph)
	}

	// Sorted predecessor outputs
	predNames := make([]string, 0, len(h.PredecessorOutputs))
	for name := range h.PredecessorOutputs {
		predNames = append(predNames, name)
	}
	sort.Strings(predNames)
	for _, name := range predNames {
		outputs := h.PredecessorOutputs[name]
		outKeys := make([]string, 0, len(outputs))
		for k := range outputs {
			outKeys = append(outKeys, k)
		}
		sort.Strings(outKeys)
		for _, k := range outKeys {
			w(digest, "pred_output:%s:%s=%s\n", name, k, outputs[k])
		}
	}

	// Sorted run params
	paramKeys := make([]string, 0, len(h.RunParams))
	for k := range h.RunParams {
		paramKeys = append(paramKeys, k)
	}
	sort.Strings(paramKeys)
	for _, k := range paramKeys {
		w(digest, "param:%s=%s\n", k, h.RunParams[k])
	}

	w(digest, "cache_version:%d\n", h.CacheVersion)

	return hex.EncodeToString(digest.Sum(nil))
}

// w writes a formatted string to a hash.Hash. hash.Hash.Write never returns
// an error so the error is intentionally discarded.
func w(h hash.Hash, format string, args ...any) {
	_, _ = fmt.Fprintf(h, format, args...)
}
