//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"time"
)

// Credentials the integration servers are started with (justfile
// `integration-up*`): CAESIUM_REGISTRY_AUTH maps the 127.0.0.1 registry host to
// secret://env/CAESIUM_IT_REGISTRY_CREDS, whose value is this user:password.
const (
	itRegistryUser     = "ci-user"
	itRegistryPassword = "ci-pass"
	itRegistryDigest   = "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
)

// privateRegistryStub emulates the Docker Registry HTTP API v2 surface of a
// bearer-token-protected private registry: an unauthenticated manifest request
// gets a 401 + WWW-Authenticate challenge, the token endpoint requires the
// operator's basic credentials, and an authenticated manifest HEAD returns
// Docker-Content-Digest. It binds inside the test-runner process, which shares
// the caesium server's network namespace on the docker + podman lanes, so the
// server reaches it at 127.0.0.1:<port>.
type privateRegistryStub struct {
	srv   *httptest.Server
	token string

	mu            sync.Mutex
	tokenBasic    []string // "user:pass" per token request
	manifestAuths []string // Authorization header per manifest request
}

func newPrivateRegistryStub() *privateRegistryStub {
	stub := &privateRegistryStub{token: fmt.Sprintf("it-token-%d", time.Now().UnixNano())}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		stub.mu.Lock()
		if ok {
			stub.tokenBasic = append(stub.tokenBasic, user+":"+pass)
		} else {
			stub.tokenBasic = append(stub.tokenBasic, "")
		}
		stub.mu.Unlock()
		if !ok || user != itRegistryUser || pass != itRegistryPassword {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"token": stub.token})
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		stub.manifestAuths = append(stub.manifestAuths, r.Header.Get("Authorization"))
		stub.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+stub.token {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="caesium-it-registry",scope="repository:private/app:pull"`, stub.srv.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/v2/private/app/manifests/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
		w.Header().Set("Docker-Content-Digest", itRegistryDigest)
		w.WriteHeader(http.StatusOK)
	})
	stub.srv = httptest.NewServer(mux)
	return stub
}

// host is the registry host:port as written in an image reference.
func (p *privateRegistryStub) host() string {
	return strings.TrimPrefix(p.srv.URL, "http://")
}

func (p *privateRegistryStub) observations() (tokenBasic, manifestAuths []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.tokenBasic...), append([]string(nil), p.manifestAuths...)
}

// TestPinDigestsPrivateRegistryAuth is the end-to-end gate for issue #405:
// with cache.pinDigests on, an image on a private, token-protected registry
// that the engine does NOT already have locally must still resolve to its
// content digest — through the engine-independent registry client, using the
// credentials the operator supplied via CAESIUM_REGISTRY_AUTH -> secret://
// provider. Before this landed the resolution failed anonymously and the task
// was silently unpinned (tag-based cache key) on every engine, and Podman /
// Kubernetes had no pre-run digest source at all.
//
// The stub serves manifests only, so the engine's own pull of the image fails
// and the task fails after identity resolution — that is expected and
// irrelevant here: the digest is persisted on the task_runs row at hash time,
// before the container is created, which is exactly what the run JSON proves.
//
// Lane coverage: docker (the Docker path finds the image absent locally and
// falls through to the registry client) and podman (the registry client is
// the engine's only digest source). Skipped on kubernetes, where the server
// runs in a kind pod that cannot reach the test process's loopback listener.
func (s *IntegrationTestSuite) TestPinDigestsPrivateRegistryAuth() {
	if s.engineType == "kubernetes" {
		s.T().Skipf("the private-registry stub runs in the test process and is not reachable from the caesium server pod under CAESIUM_TEST_ENGINE=%s; covered on the docker + podman lanes, where the test runner shares the server's network namespace", s.engineType)
	}

	stub := newPrivateRegistryStub()
	defer stub.srv.Close()

	imageRef := stub.host() + "/private/app:1.0"
	alias := fmt.Sprintf("integration-registry-auth-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  cache:
    pinDigests: true
    digestTTL: 0
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: private
    image: %s
    command: ["sh", "-c", "echo hello"]
`, alias, imageRef)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.T().Logf("run %s finished with status %q (the stub serves no layers, so the engine's own pull is expected to fail)", runID, run.Status)

	// The real surface: the run's task rows carry the digest the server
	// resolved before it tried to create the container.
	var detail struct {
		Tasks []struct {
			ID                  string `json:"id"`
			Status              string `json:"status"`
			ResolvedImageDigest string `json:"resolved_image_digest"`
		} `json:"tasks"`
	}
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s", job.ID, runID), &detail)
	s.Require().Len(detail.Tasks, 1)
	s.Equal(itRegistryDigest, detail.Tasks[0].ResolvedImageDigest,
		"a private-registry image must resolve to the digest served by the registry (engine=%s), not fall back to the tag", s.engineType)

	// Corroborate on the second read surface: the reproduce descriptor's
	// runtime block carries the same digest (what `caesium reproduce` pins).
	status, body := s.getReproduceDescriptor(job.ID, runID, "private")
	s.Require().Equal(http.StatusOK, status, string(body))
	var resp reproduceDescriptorResponse
	s.Require().NoError(json.Unmarshal(body, &resp))
	var desc reproduceDescriptorPayload
	s.Require().NoError(json.Unmarshal(resp.Descriptor, &desc))
	s.Equal(itRegistryDigest, desc.Runtime.ResolvedImageDigest, "the reproduce descriptor must record the registry-resolved digest")

	// And the server got there the way an operator would expect: it answered
	// the bearer challenge by exchanging the configured credentials for a
	// token, then fetched the manifest with that token. No anonymous retry
	// storm: exactly one challenge, one token exchange, one authenticated HEAD.
	tokenBasic, manifestAuths := stub.observations()
	s.Require().NotEmpty(tokenBasic, "the server must have hit the token endpoint")
	s.Equal([]string{itRegistryUser + ":" + itRegistryPassword}, tokenBasic,
		"the token exchange must carry the credentials resolved from secret://env/CAESIUM_IT_REGISTRY_CREDS, exactly once")
	s.Require().Len(manifestAuths, 2, "one unauthenticated probe (challenge) and one authenticated manifest request")
	s.Equal("", manifestAuths[0])
	s.Equal("Bearer "+stub.token, manifestAuths[1])
}
