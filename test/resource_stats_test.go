//go:build integration

package test

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"os"
	"path"
	"time"

	"github.com/containers/podman/v5/pkg/bindings"
	podman "github.com/containers/podman/v5/pkg/bindings/containers"
	"github.com/containers/podman/v5/pkg/domain/entities/types"
	docker "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

type resourceTaskObservation struct {
	ID              string   `json:"id"`
	TaskRunID       string   `json:"task_run_id"`
	Value           string   `json:"value"`
	RuntimeID       string   `json:"runtime_id"`
	Status          string   `json:"status"`
	Result          string   `json:"result"`
	ExitCode        *int     `json:"exit_code"`
	PeakMemoryBytes *int64   `json:"peak_memory_bytes"`
	CPUSeconds      *float64 `json:"cpu_seconds"`
	StatsSource     string   `json:"stats_source"`
	OOMKilled       bool     `json:"oom_killed"`
}
type resourceRunObservation struct {
	Tasks []resourceTaskObservation `json:"tasks"`
}
type resourcePartitionObservations struct {
	Partitions []resourceTaskObservation `json:"partitions"`
}

// Limits are injected by the test into its waiting fixture container. This
// exercises the shipped apply/run/executor/inspect/persistence/REST wiring before
// Plan2 B adds declarative resources; it adds no product limit or resize path.
type resourceFixtureRuntime struct {
	s      *IntegrationTestSuite
	ctx    context.Context
	cancel context.CancelFunc
	docker *client.Client
	ids    map[string]bool
}

func (s *IntegrationTestSuite) resourceFixtureRuntime() *resourceFixtureRuntime {
	s.T().Helper()
	if s.engineType == "kubernetes" {
		s.T().Skip("resource OOM fault injection is Docker/Podman; kind limits and live degradation are Plan2 H-2 after B2")
	}
	ctx, cancel := context.WithTimeout(s.T().Context(), 3*time.Minute)
	r := &resourceFixtureRuntime{s: s, ctx: ctx, cancel: cancel, ids: map[string]bool{}}
	if s.engineType == "podman" {
		var err error
		r.ctx, err = bindings.NewConnection(ctx, os.Getenv("CAESIUM_PODMAN_URI"))
		s.Require().NoError(err)
	} else {
		r.docker = s.dockerClient()
	}
	s.T().Cleanup(func() {
		for id := range r.ids {
			if r.docker != nil {
				_ = r.docker.ContainerRemove(context.WithoutCancel(r.ctx), id, docker.RemoveOptions{Force: true})
			} else {
				_, _ = podman.Remove(context.WithoutCancel(r.ctx), id, &podman.RemoveOptions{Force: new(true)})
			}
		}
		if r.docker != nil {
			_ = r.docker.Close()
		}
		r.cancel()
	})
	return r
}

func (r *resourceFixtureRuntime) release(id, waitFile string, memoryMiB int64) {
	r.s.T().Helper()
	r.s.Require().NotEmpty(id)
	r.ids[id] = true
	limit := memoryMiB * 1024 * 1024
	if r.docker != nil {
		_, err := r.docker.ContainerUpdate(r.ctx, id, docker.UpdateConfig{Resources: docker.Resources{Memory: limit, MemorySwap: limit}})
		r.s.Require().NoError(err)
		inspect, err := r.docker.ContainerInspect(r.ctx, id)
		r.s.Require().NoError(err)
		r.s.Require().Equal(limit, inspect.HostConfig.Memory)
		r.s.Require().Equal(limit, inspect.HostConfig.MemorySwap)

	} else {
		// Podman 4.9, the Ubuntu 24.04 CI server, applies a live update to the
		// OCI runtime without rewriting the stored spec, so its inspect keeps
		// reporting the create-time 0 while Podman 5.x rewrites it. There is no
		// engine reading of the applied limit that holds on both, so the limit
		// is verified from inside the workload by requireInjectedLimit.
		_, err := podman.Update(r.ctx, &types.ContainerUpdateOptions{NameOrID: id, Resources: &specs.LinuxResources{Memory: &specs.LinuxMemory{Limit: &limit, Swap: &limit}}})
		r.s.Require().NoError(err)

	}

	// Copy the release file without starting another process in the workload's
	// cgroup. An exec monitor can race OOM notification/cleanup for the same
	// container; the fixture must only measure its original runtime process.
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	r.s.Require().NoError(writer.WriteHeader(&tar.Header{Name: path.Base(waitFile), Mode: 0o644, Size: 0}))
	r.s.Require().NoError(writer.Close())
	if r.docker != nil {
		r.s.Require().NoError(r.docker.CopyToContainer(r.ctx, id, path.Dir(waitFile), &archive, docker.CopyToContainerOptions{}))
	} else {
		copyFile, err := podman.CopyFromArchive(r.ctx, id, path.Dir(waitFile), &archive)
		r.s.Require().NoError(err)
		r.s.Require().NoError(copyFile())
	}
}

// requireInjectedLimit asserts the workload itself saw the harness's limit on
// its own memory cgroup. The fixture prints that reading once the release
// barrier lifts, which is the only observation of an injected limit that holds
// on every engine and server version the lanes run — and, unlike an extra exec
// probe against the live container, it cannot perturb the runtime whose OOM
// evidence is under test.
func (s *IntegrationTestSuite) requireInjectedLimit(logText string, memoryMiB int64) {
	s.T().Helper()
	s.Contains(logText, fmt.Sprintf("cgroup memory limit %d", memoryMiB*1024*1024),
		"the harness must inject its memory limit into the workload's own cgroup")
}

func (s *IntegrationTestSuite) TestResourceStatsCapturesAttempt() {
	for _, tc := range []struct {
		name   string
		memory int
		oom    bool
	}{{"sampled", 16, false}, {"oom", 128, true}} {
		s.Run(tc.name, func() {
			runtime := s.resourceFixtureRuntime()
			alias := fmt.Sprintf("resource-stats-%s-%d", tc.name, time.Now().UnixNano())
			waitFile := "/tmp/" + alias
			manifest := fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: measure
    image: %s
    cache: {enabled: false}
    command: ["--memory-mib", "%d", "--wait-file", "%s", "--hold", "3s"]
`, alias, resourceStressImage(), tc.memory, waitFile)
			dir := s.writeJobManifest(manifest)
			defer os.RemoveAll(dir)
			s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
			job := s.requireJobByAlias(alias)
			runID := s.triggerRun(job.ID)
			var observation resourceRunObservation
			s.Require().Eventually(func() bool {
				if err := s.tryGetJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s", job.ID, runID), &observation); err != nil {
					return false
				}
				return len(observation.Tasks) == 1 && observation.Tasks[0].RuntimeID != ""
			}, 60*time.Second, 100*time.Millisecond, "fixture runtime never started")
			runtime.release(observation.Tasks[0].RuntimeID, waitFile, 64)
			completed := s.awaitRun(job.ID, runID, runTimeout)
			s.getJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s", job.ID, runID), &observation)
			s.Require().Len(observation.Tasks, 1)
			task := observation.Tasks[0]
			s.requireInjectedLimit(s.taskLog(job.ID, runID, s.jobTaskIDByName(job.ID, "measure")), 64)
			s.Require().NotNil(task.ExitCode)
			s.Require().NotNil(task.PeakMemoryBytes, "runtime observation must survive the full server read path")
			s.Equal(tc.oom, task.OOMKilled)
			if tc.oom {
				s.Equal("failed", completed.Status)
				s.Equal("resource_failure", task.Result)
				s.Equal(137, *task.ExitCode)
				s.requireOOMMemoryObservation(task, 64)
				s.Contains([]string{"oom_inferred", "sampled"}, task.StatsSource)
				if s.authAPIKey != "" {
					s.Require().Eventually(func() bool {
						var list approvalIncidentList
						if s.tryGetJSON("/v1/incidents?job_id="+job.ID, &list) != nil {
							return false
						}
						for _, inc := range list.Incidents {
							if inc.TaskName == "measure" {
								return inc.Class == "oom"
							}
						}
						return false
					}, 15*time.Second, 100*time.Millisecond, "runtime OOM must classify oom, not transient_infra")
				}
			} else {
				s.Equal("succeeded", completed.Status)
				s.Equal(0, *task.ExitCode)
				s.Equal("sampled", task.StatsSource)
				s.GreaterOrEqual(*task.PeakMemoryBytes, int64(16*1024*1024))
				s.Require().NotNil(task.CPUSeconds)
				s.Greater(*task.CPUSeconds, 0.0)
			}
		})
	}
}

func (s *IntegrationTestSuite) TestResourceStatsFanOutKeepsInstanceObservations() {
	runtime := s.resourceFixtureRuntime()
	alias := fmt.Sprintf("resource-stats-fanout-%d", time.Now().UnixNano())
	waitFile := "/tmp/" + alias
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: list
    image: alpine:3.23
    cache: {enabled: false}
    command: ["sh", "-c", "echo '##caesium::partitions [\"healthy\",\"oom\"]'"]
    next: [measure]
  - name: measure
    image: %s
    cache: {enabled: false}
    command: ["--memory-mib", "128", "--wait-file", "%s", "--hold", "3s"]
    dependsOn: [list]
    fanOut:
      from: list
      maxPartitions: 2
      failurePolicy: continue
`, alias, resourceStressImage(), waitFile)
	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	path := fmt.Sprintf("/v1/jobs/%s/runs/%s/tasks/measure/partitions", job.ID, runID)
	released := map[string]bool{}
	// Release each instance as it appears, supporting single-slot workers as
	// well as parallel dispatch without waiting for an impossible overlap.
	s.Require().Eventually(func() bool {
		var out resourcePartitionObservations
		if s.tryGetJSON(path, &out) != nil {
			return false
		}
		for _, task := range out.Partitions {
			if task.Value == "" || task.RuntimeID == "" || released[task.Value] {
				continue
			}
			limit := int64(256)
			if task.Value == "oom" {
				limit = 64
			}
			runtime.release(task.RuntimeID, waitFile, limit)
			released[task.Value] = true
		}
		return len(released) == 2
	}, 90*time.Second, 100*time.Millisecond, "both instance workloads must execute")
	s.awaitRun(job.ID, runID, runTimeout)
	var out resourcePartitionObservations
	s.getJSON(path, &out)
	byValue := map[string]resourceTaskObservation{}
	for _, task := range out.Partitions {
		if task.Value != "" {
			byValue[task.Value] = task
		}
	}
	s.Require().Len(byValue, 2)
	healthy, oom := byValue["healthy"], byValue["oom"]
	measure := s.jobTaskIDByName(job.ID, "measure")
	s.requireInjectedLimit(s.taskRunLog(job.ID, runID, measure, healthy.TaskRunID), 256)
	s.requireInjectedLimit(s.taskRunLog(job.ID, runID, measure, oom.TaskRunID), 64)
	s.Equal("succeeded", healthy.Status)
	s.False(healthy.OOMKilled)
	s.Equal("failed", oom.Status)
	s.True(oom.OOMKilled)
	s.Equal("resource_failure", oom.Result)
	s.Require().NotNil(healthy.PeakMemoryBytes)
	s.Require().NotNil(oom.PeakMemoryBytes)
	s.GreaterOrEqual(*healthy.PeakMemoryBytes, int64(128*1024*1024))
	s.requireOOMMemoryObservation(oom, 64)
	s.NotEqual(*healthy.PeakMemoryBytes, *oom.PeakMemoryBytes, "sibling outcomes must not overwrite one another")
	s.NotEqual(healthy.TaskRunID, oom.TaskRunID)
}

// A sampled peak can miss a fast OOM. Only an inspected limit is a justified
// lower bound. Podman 4.9 does not persist a live update into that inspect spec.
func (s *IntegrationTestSuite) requireOOMMemoryObservation(task resourceTaskObservation, limitMiB int64) {
	s.T().Helper()
	s.Require().NotNil(task.PeakMemoryBytes)
	if s.engineType == "podman" && task.StatsSource == "sampled" {
		s.Greater(*task.PeakMemoryBytes, int64(0), "retain actual samples when the terminal limit is unavailable")
	} else {
		s.GreaterOrEqual(*task.PeakMemoryBytes, limitMiB*1024*1024)
	}
}
