package kubernetes

import (
	"encoding/base64"
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

func TestGetKubernetesCoreRejectsMissingExternalCAFile(t *testing.T) {
	dir := t.TempDir()
	kubeDir := filepath.Join(dir, ".kube")
	if err := os.Mkdir(kubeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const kubeconfig = `apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority: ca.crt
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

	_, err := getKubernetesCore(dir)
	if err == nil {
		t.Fatal("expected getKubernetesCore to fail when certificate-authority file is missing")
	}
}

func TestGetKubernetesCoreLoadsFlattenedCertificateAuthorityData(t *testing.T) {
	dir := t.TempDir()
	kubeDir := filepath.Join(dir, ".kube")
	if err := os.Mkdir(kubeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Throwaway self-signed cert used only to prove client-go accepts
	// certificate-authority-data (the form the CLI wrapper emits after
	// flattening a sibling ca.crt). No ca.crt file is next to the config.
	const testCA = `-----BEGIN CERTIFICATE-----
MIIDDzCCAfegAwIBAgIUaExcJA6PpkjP1ZHbzF8RMzaRhbIwDQYJKoZIhvcNAQEL
BQAwFzEVMBMGA1UEAwwMY2Flc2l1bS10ZXN0MB4XDTI2MDkxNzAyMDcxNVoXDTI2
MDkxODAyMDcxNVowFzEVMBMGA1UEAwwMY2Flc2l1bS10ZXN0MIIBIjANBgkqhkiG
9w0BAQEFAAOCAQ8AMIIBCgKCAQEAqXNae3I9Y6zSrxU02Cf0b9DxYvnE8kM+qDUU
K/gDYQTn4aCfGReYVjb8MzIFIMtKmvV3ZLP1qvDBxMbZguwKYOdAjKOh+UOldwn2
K4otjQoQsHoFpnWwHbC5JmPiVPtNCjfp8EM5OqCV4mzyPx0o5Z1Gq7n/X3ooRfnk
wv8cJ+A3tPVZIoLBYWgefiKZ//FKswDG2QnBPF4OiDWqSQyN6DdevCed39cb3+Qd
xnKf9DtNES+A0ZlZx9Ehxx+jvvVIMPqCk8ETCliXn47fVhpWIhIcIlxo2nz5SvsQ
DzhVTwojD2k/08KOZE1TLhp7b4ZGuSXFhT/cnWiO6iiTybQXlQIDAQABo1MwUTAd
BgNVHQ4EFgQUX9CkGWxbBkMufqoDmMCDjLNSIvowHwYDVR0jBBgwFoAUX9CkGWxb
BkMufqoDmMCDjLNSIvowDwYDVR0TAQH/BAUwAwEB/zANBgkqhkiG9w0BAQsFAAOC
AQEAVmwiVD8D9QLZ7MHWTzNBpZSnK4YGLd2NUEXYzZ3u/c1HtFTh447ouD7qnzXC
0ELaOScRZJc1iDuVP6tfMwNniYrUeknmj+3rCFm0w3DQkEZdp5w7rWzR5kdPV5Qs
/C7OxgSwB9kuiRcmMKH7uXotyoi0dRfh+ftweyYigcKqamJvVqA6Ro1EOoB19uTC
Pu1M/Sd7YMgDm6DqsQJGhlGOgc6ufILZTWFpUZrq6t0C9URKrcjJrhIS7kvfujoa
GwpLke+ey5vw/0IoXCe+MR6C/GqzneAoFwQbZoFpHvh9nc99Nr0JidpWks2hfAU4
s8YqKStVyPw7mjMKbcb5qRHW9Q==
-----END CERTIFICATE-----
`
	kubeconfig := "apiVersion: v1\nkind: Config\nclusters:\n- cluster:\n    certificate-authority-data: " +
		base64.StdEncoding.EncodeToString([]byte(testCA)) +
		"\n    server: https://127.0.0.1:1\n  name: test\ncontexts:\n- context:\n    cluster: test\n    user: test\n  name: test\ncurrent-context: test\nusers:\n- name: test\n  user:\n    token: dummy\n"
	if err := os.WriteFile(filepath.Join(kubeDir, "config"), []byte(kubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}

	core, err := getKubernetesCore(dir)
	if err != nil {
		t.Fatalf("getKubernetesCore with flattened CA data: %v", err)
	}
	if core == nil {
		t.Fatal("expected a CoreV1 client from flattened certificate-authority-data")
	}
}
