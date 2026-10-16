//go:build integration

package test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/connector"
	"github.com/caesium-cloud/caesium/test/robustness/cluster"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// TestConnectorsConfigCluster uses the existing distributed kind/Helm fixture.
// The orchestrator installs one shared, real Raft catalog and three actual
// Caesium replicas. No fake HTTP provider or hand-seeded catalog can qualify it.
// Running independently of IntegrationTestSuite avoids a localhost CLI/server
// assumption in the in-cluster integration runner.
func TestConnectorsConfigCluster(t *testing.T) {
	if os.Getenv("CAESIUM_CONNECTORS_CONFIG_CLUSTER") != "true" {
		t.Skip("requires selected real distributed connector fixture")
	}
	ns := os.Getenv("POD_NAMESPACE")
	require.NotEmpty(t, ns)
	cmName := os.Getenv("CAESIUM_CONNECTORS_CLUSTER_CONFIGMAP")
	require.NotEmpty(t, cmName)
	key := os.Getenv("CAESIUM_AUTH_ADMIN_KEY")
	require.NotEmpty(t, key)
	candidate := os.Getenv("CAESIUM_CONNECTORS_CANDIDATE_SHA")
	require.Len(t, candidate, 40)
	digest := os.Getenv("CAESIUM_CONNECTORS_CLUSTER_CANDIDATE_DIGEST")
	require.NotEmpty(t, digest)
	artifacts := os.Getenv("CAESIUM_CONNECTORS_ARTIFACTS")
	require.True(t, filepath.IsAbs(artifacts))
	kube, err := cluster.InClusterClient()
	require.NoError(t, err)
	topology, err := cluster.DiscoverTopology(t.Context(), kube, ns)
	require.NoError(t, err)
	require.Len(t, topology.Members, 3, "matching files must be proved on real shared-catalog replicas")
	memberEvidence := []map[string]string{}
	uids, ips, pvcs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, member := range topology.Members {
		require.NotEmpty(t, member.UID)
		require.NotEmpty(t, member.IP)
		require.NotEmpty(t, member.PVCName)
		require.NotEmpty(t, member.VolumeName)
		require.NotEmpty(t, member.ContainerID)
		require.False(t, uids[member.UID])
		require.False(t, ips[member.IP])
		require.False(t, pvcs[member.PVCName])
		uids[member.UID], ips[member.IP], pvcs[member.PVCName] = true, true, true
		require.True(t, cluster.PodReady(&member.Pod), member.Name)
		require.True(t, cluster.ImageIDMatchesCandidate(member.ImageID, digest), "member %s image %q != candidate %q", member.Name, member.ImageID, digest)
		memberEvidence = append(memberEvidence, map[string]string{"name": member.Name, "uid": member.UID, "ip": member.IP, "node": member.Node, "pvc": member.PVCName, "volume": member.VolumeName, "image_id": member.ImageID, "container_id": member.ContainerID})
	}
	a, b := topology.Members[0], topology.Members[1]
	cm, err := kube.CoreV1().ConfigMaps(ns).Get(t.Context(), cmName, metav1.GetOptions{})
	require.NoError(t, err)
	base := cm.Data[a.Name+".yaml"]
	require.NotEmpty(t, base)
	require.Equal(t, base, cm.Data[b.Name+".yaml"])
	var initial connectorReadConnections
	connectorReadHTTP(t, a.HTTPBase(), key, http.MethodGet, "/v1/connectors", nil, &initial)
	require.True(t, initial.Ready)
	var matching connectorReadConnections
	connectorReadHTTP(t, b.HTTPBase(), key, http.MethodGet, "/v1/connectors", nil, &matching)
	require.Equal(t, initial.Fingerprint, matching.Fingerprint)
	// Create the external execution via the real SDK worker control, then let
	// two Caesium processes independently inspect the same immutable reference.
	var ref fixtureRef
	connectorReadHTTP(t, connectorFixtureControlURL(), "", http.MethodPost, "/start", map[string]any{"id": "cluster-config-" + uuid.NewString(), "type": "external_only"}, &ref)
	require.NotEmpty(t, ref.RunID)
	reference, err := connector.NewExecutionReference("fixture", map[string]string{"workflow_id": ref.WorkflowID, "run_id": ref.RunID})
	require.NoError(t, err)
	// Route IDs come from public discovery. The locally built reference only
	// supplies an independent immutable identity assertion and creates no rows.
	var listed connectorReadPage
	require.Eventually(t, func() bool {
		connectorReadHTTP(t, a.HTTPBase(), key, http.MethodGet, "/v1/connectors/fixture/executions?workflow_type=external_only&page_size=100", nil, &listed)
		for _, execution := range listed.Executions {
			if execution.Metadata.RunID == ref.RunID {
				return true
			}
		}
		return false
	}, 20*time.Second, 500*time.Millisecond)
	var routeID string
	for _, execution := range listed.Executions {
		if execution.Metadata.RunID == ref.RunID {
			routeID = execution.ID
			require.Equal(t, reference.Coordinates, execution.Reference.Coordinates)
		}
	}
	require.NotEmpty(t, routeID)
	path := "/v1/connectors/fixture/executions/" + url.PathEscape(routeID)
	beforeMatch := connectorProviderRPCs(t)
	for _, member := range []cluster.Member{a, b} {
		var detail connectorReadExecution
		connectorReadHTTP(t, member.HTTPBase(), key, http.MethodGet, path, nil, &detail)
		require.Equal(t, ref.RunID, detail.Metadata.RunID)
		require.Equal(t, reference.Coordinates, detail.Reference.Coordinates)
	}
	require.Greater(t, connectorProviderRPCs(t), beforeMatch, "matching processes must reach the real provider frontend")
	changed := connectorClusterConfig(t, base, false)
	changedConfig, err := connector.Parse([]byte(changed), nil)
	require.NoError(t, err)
	next, err := connector.Fingerprint(changedConfig)
	require.NoError(t, err)
	require.NotEqual(t, initial.Fingerprint, next)
	steps := []map[string]any{{"phase": "matching", "fingerprint": initial.Fingerprint, "members": []string{a.Name, b.Name}, "provider_rpc_count": connectorProviderRPCs(t)}}
	// A harmless limit change is still a genuine configuration mismatch. It
	// must not implicitly adopt the stored epoch or interfere with local jobs.
	beforeMismatch := connectorProviderRPCs(t)
	connectorClusterSetConfig(t, kube, ns, cmName, b.Name, changed, "")
	b = connectorClusterRestart(t, kube, ns, b)
	require.Equal(t, beforeMismatch, connectorProviderRPCs(t), "mismatched startup must not dial or send a provider RPC")
	connectorClusterRefused(t, b.HTTPBase(), key, path)
	steps = append(steps, map[string]any{"phase": "mismatch_refused", "member": b.Name, "uid": b.UID, "provider_rpc_count": connectorProviderRPCs(t)})
	// The expected-previous value is supplied explicitly, never learned or
	// adopted by startup. This activates the new epoch in the shared catalog.
	connectorClusterSetConfig(t, kube, ns, cmName, b.Name, changed, initial.Fingerprint)
	b = connectorClusterRestart(t, kube, ns, b)
	var transitioned connectorReadConnections
	connectorReadHTTP(t, b.HTTPBase(), key, http.MethodGet, "/v1/connectors", nil, &transitioned)
	require.True(t, transitioned.Ready)
	require.Equal(t, next, transitioned.Fingerprint)
	var detail connectorReadExecution
	connectorReadHTTP(t, b.HTTPBase(), key, http.MethodGet, path, nil, &detail)
	require.Equal(t, ref.RunID, detail.Metadata.RunID)
	// A process which was already serving the old epoch must recheck it for
	// every request, including an existing client/pooled transport's direct read.
	connectorClusterRefused(t, a.HTTPBase(), key, path)
	steps = append(steps, map[string]any{"phase": "guarded_transition_and_stale_node", "fingerprint": next, "active_member": b.Name, "stale_member": a.Name, "provider_rpc_count": connectorProviderRPCs(t)})
	connectorClusterSetConfig(t, kube, ns, cmName, a.Name, changed, "")
	a = connectorClusterRestart(t, kube, ns, a)
	connectorReadHTTP(t, a.HTTPBase(), key, http.MethodGet, path, nil, &detail)
	require.Equal(t, ref.RunID, detail.Metadata.RunID)
	// Real inspected references now exist. Naming the current epoch does not
	// permit changing the immutable endpoint behind an existing connection ID.
	repointed := connectorClusterConfig(t, changed, true)
	beforeRepoint := connectorProviderRPCs(t)
	connectorClusterSetConfig(t, kube, ns, cmName, b.Name, repointed, next)
	b = connectorClusterRestart(t, kube, ns, b)
	require.Equal(t, beforeRepoint, connectorProviderRPCs(t), "repointed startup must refuse before any provider RPC")
	connectorClusterRefused(t, b.HTTPBase(), key, path)
	connectorReadHTTP(t, a.HTTPBase(), key, http.MethodGet, "/v1/connectors", nil, &transitioned)
	require.Equal(t, next, transitioned.Fingerprint, "refused repoint must not advance the shared catalog epoch")
	steps = append(steps, map[string]any{"phase": "immutable_repoint_refused", "member": b.Name, "uid": b.UID, "fingerprint": transitioned.Fingerprint, "provider_rpc_count": connectorProviderRPCs(t)})
	require.NoError(t, os.WriteFile(filepath.Join(artifacts, "config-cluster-evidence.json"), mustConnectorJSON(t, map[string]any{"candidate_sha": candidate, "candidate_image_digest": digest, "namespace": ns, "initial_members": memberEvidence, "workflow": ref, "steps": steps}), 0600))
}

func connectorClusterConfig(t *testing.T, raw string, repoint bool) string {
	t.Helper()
	var document map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(raw), &document))
	connections, ok := document["connections"].([]any)
	require.True(t, ok)
	require.Len(t, connections, 1)
	c, ok := connections[0].(map[string]any)
	require.True(t, ok)
	if repoint {
		endpoint, ok := c["endpoint"].(string)
		require.True(t, ok)
		host, _, err := net.SplitHostPort(endpoint)
		require.NoError(t, err)
		c["endpoint"] = net.JoinHostPort(host, "7235")
	} else {
		limits, _ := c["limits"].(map[string]any)
		if limits == nil {
			limits = map[string]any{}
		}
		limits["readsPerMinute"] = 59
		c["limits"] = limits
	}
	out, err := yaml.Marshal(document)
	require.NoError(t, err)
	return string(out)
}

func connectorClusterSetConfig(t *testing.T, kube *kubernetes.Clientset, ns, name, pod, config, previous string) {
	t.Helper()
	cm, err := kube.CoreV1().ConfigMaps(ns).Get(t.Context(), name, metav1.GetOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, cm.UID)
	require.NotEmpty(t, cm.Data[pod+".yaml"], "only mutate an explicitly provisioned member's configuration")
	cm.Data[pod+".yaml"] = config
	if previous == "" {
		delete(cm.Data, pod+".previous")
	} else {
		cm.Data[pod+".previous"] = previous
	}
	_, err = kube.CoreV1().ConfigMaps(ns).Update(t.Context(), cm, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func connectorClusterRestart(t *testing.T, kube *kubernetes.Clientset, ns string, old cluster.Member) cluster.Member {
	t.Helper()
	uid := types.UID(old.UID)
	require.NotEmpty(t, uid)
	require.NoError(t, kube.CoreV1().Pods(ns).Delete(t.Context(), old.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}))
	var replacement corev1.Pod
	require.Eventually(t, func() bool {
		pod, err := kube.CoreV1().Pods(ns).Get(t.Context(), old.Name, metav1.GetOptions{})
		if err != nil || pod.UID == uid || !cluster.PodReady(pod) || pod.Status.PodIP == "" {
			return false
		}
		replacement = *pod
		return connectorClusterStatus(t.Context(), "http://"+net.JoinHostPort(pod.Status.PodIP, "8080")+"/health/ready") == http.StatusOK
	}, 3*time.Minute, time.Second, "replacement process must regain local catalog readiness")
	topology, err := cluster.DiscoverTopology(t.Context(), kube, ns)
	require.NoError(t, err)
	member, found := topology.ByName(old.Name)
	require.True(t, found)
	require.Equal(t, string(replacement.UID), member.UID)
	require.NotEqual(t, old.UID, member.UID)
	require.NotEqual(t, old.ContainerID, member.ContainerID)
	require.Equal(t, old.PVCName, member.PVCName, "configuration restart must retain the same catalog PVC")
	require.Equal(t, old.VolumeName, member.VolumeName)
	require.Equal(t, old.ImageID, member.ImageID, "every replacement must run the candidate image")
	return member
}

func connectorClusterStatus(ctx context.Context, target string) int {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0
	}
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return 0
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

func connectorClusterRefused(t *testing.T, base, key, detail string) {
	t.Helper()
	require.Equal(t, http.StatusOK, connectorClusterStatus(t.Context(), base+"/health/ready"), "connector mismatch must preserve local job readiness")
	before := connectorProviderRPCs(t)
	for _, path := range []string{"/v1/connectors", "/v1/connectors/fixture/executions", detail, detail + "/history", detail + "/relationships"} {
		status, raw := connectorReadHTTP(t, base, key, http.MethodGet, path, nil, nil)
		require.Equal(t, http.StatusServiceUnavailable, status, "%s: %s", path, raw)
	}
	require.Equal(t, before, connectorProviderRPCs(t), "mismatched, stale and repointed processes must refuse before any provider RPC")
	var features map[string]any
	connectorReadHTTP(t, base, key, http.MethodGet, "/v1/system/features", nil, &features)
	require.Equal(t, true, features["connectors_enabled"])
	require.Equal(t, false, features["connectors_ready"])
	// Do not retain any PodSpec environment values in evidence artifacts.
	t.Logf("observed connector-only refusal at %s with zero additional provider RPCs (%d)", strings.TrimPrefix(base, "http://"), before)
}
