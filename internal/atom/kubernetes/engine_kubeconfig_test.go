package kubernetes

import (
	"os"
	"path/filepath"
	"testing"
)

// The CLI wrapper mounts the host kubeconfig at
// $CAESIUM_KUBERNETES_CONFIG/.kube/config. getKubernetesCore must load that
// layout (not KUBECONFIG).
func TestGetKubernetesCoreLoadsDotKubeUnderConfigDir(t *testing.T) {
	dir := t.TempDir()
	kubeDir := filepath.Join(dir, ".kube")
	if err := os.Mkdir(kubeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const kubeconfig = `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:1
  name: test
contexts:
- context:
    cluster: test
    user: test
  name: test
current-context: test
users:
- name: test
  user:
    token: dummy
`
	if err := os.WriteFile(filepath.Join(kubeDir, "config"), []byte(kubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}

	core, err := getKubernetesCore(dir)
	if err != nil {
		t.Fatalf("getKubernetesCore(%q): %v", dir, err)
	}
	if core == nil {
		t.Fatal("expected a CoreV1 client from the mounted kubeconfig layout")
	}
}
