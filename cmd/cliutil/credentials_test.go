package cliutil

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func isolateConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv(APIKeyEnvVar, "")
	return dir
}

func TestCredentialsDirFallsBackToDotConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	dir, err := CredentialsDir()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, ".config", "caesium"), dir)

	t.Setenv("XDG_CONFIG_HOME", "relative/path")
	dir, err = CredentialsDir()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, ".config", "caesium"), dir, "a relative XDG_CONFIG_HOME is ignored")

	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir, err = CredentialsDir()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(xdg, "caesium"), dir)
}

func TestCanonicalServer(t *testing.T) {
	for in, want := range map[string]string{
		"http://localhost:8080":           "http://localhost:8080",
		"HTTPS://Caesium.Example.com/":    "https://caesium.example.com",
		" https://x.example/prefix// ":    "https://x.example/prefix",
		"https://x.example/a?b=c#d":       "https://x.example/a",
		"http://127.0.0.1:8080/caesium/":  "http://127.0.0.1:8080/caesium",
		"https://caesium.example.com:443": "https://caesium.example.com",
		"http://caesium.example.com:80/":  "http://caesium.example.com",
		"http://caesium.example.com:443":  "http://caesium.example.com:443",
		"https://caesium.example.com:80":  "https://caesium.example.com:80",
		"HTTP://[::1]:80/x":               "http://[::1]/x",
		"http://[::1]:8080":               "http://[::1]:8080",
		"https://user:pw@host.example/a":  "https://host.example/a",
		// Escaped path characters stay escaped: decoding them would turn
		// %23 into a fragment, %3F into a query and %2F into a separator.
		"https://host/team%23blue":  "https://host/team%23blue",
		"https://host/team%23blue/": "https://host/team%23blue",
		"https://host/a%3Fb":        "https://host/a%3Fb",
		"https://host/a%2Fb/":       "https://host/a%2Fb",
		"https://host/a%2fb":        "https://host/a%2Fb",
		"https://host/a%2F/":        "https://host/a%2F",
		"https://host/My%20Team":    "https://host/My%20Team",
		"https://host/My Team":      "https://host/My%20Team",
		"https://HOST/Case/Kept":    "https://host/Case/Kept",
	} {
		got, err := CanonicalServer(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
	for _, bad := range []string{"", "localhost:8080", "ftp://x", "/relative"} {
		_, err := CanonicalServer(bad)
		require.Error(t, err, bad)
	}
}

func TestCredentialStoreRoundTripAndPermissions(t *testing.T) {
	xdg := isolateConfig(t)
	store, err := LoadCredentials()
	require.NoError(t, err)
	_, ok := store.Lookup("http://localhost:8080")
	require.False(t, ok, "a missing file is an empty store")

	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	require.NoError(t, store.Put(StoredCredential{Server: "HTTP://LocalHost:8080/", Key: "csk_user_secret", KeyID: "k1", KeyPrefix: "csk_user_secr", ExpiresAt: exp, Subject: "a@b"}))
	require.NoError(t, store.Put(StoredCredential{Server: "https://other.example", Key: "csk_user_other"}))
	require.NoError(t, store.Save())

	dir := filepath.Join(xdg, "caesium")
	path := filepath.Join(dir, "credentials.yaml")
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		info, err = os.Stat(dir)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	}

	loaded, err := LoadCredentials()
	require.NoError(t, err)
	cred, ok := loaded.Lookup("http://localhost:8080")
	require.True(t, ok)
	require.Equal(t, "csk_user_secret", cred.Key)
	require.True(t, cred.ExpiresAt.Equal(exp))
	require.Equal(t, "a@b", cred.Subject)

	require.NoError(t, loaded.Put(StoredCredential{Server: "http://localhost:8080", Key: "csk_user_new"}))
	cred, _ = loaded.Lookup("http://localhost:8080")
	require.Equal(t, "csk_user_new", cred.Key, "a login replaces the server's entry")
	require.True(t, loaded.Remove("http://localhost:8080/"))
	require.False(t, loaded.Remove("http://localhost:8080"))
	_, ok = loaded.Lookup("https://other.example")
	require.True(t, ok, "other servers are untouched")

	if runtime.GOOS != "windows" {
		require.NoError(t, os.Chmod(path, 0o644))
		_, err = LoadCredentials()
		require.ErrorContains(t, err, "accessible by other users")
	}
}

// Concurrent CLI processes (a login for one server, a logout for another)
// update the file under one lock, from freshly loaded state, so none
// overwrites another's entry.
func TestUpdateCredentialsSerialisesConcurrentWriters(t *testing.T) {
	isolateConfig(t)
	const writers = 16
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- UpdateCredentials(func(s *CredentialStore) error {
				return s.Put(StoredCredential{Server: fmt.Sprintf("http://srv-%d.example", i), Key: fmt.Sprintf("k%d", i)})
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	store, err := LoadCredentials()
	require.NoError(t, err)
	for i := 0; i < writers; i++ {
		cred, ok := store.Lookup(fmt.Sprintf("http://srv-%d.example", i))
		require.True(t, ok, "writer %d's entry survived", i)
		require.Equal(t, fmt.Sprintf("k%d", i), cred.Key)
	}

	require.ErrorContains(t, UpdateCredentials(func(s *CredentialStore) error {
		s.Remove("http://srv-0.example")
		return fmt.Errorf("refused")
	}), "refused")
	store, err = LoadCredentials()
	require.NoError(t, err)
	_, ok := store.Lookup("http://srv-0.example")
	require.True(t, ok, "a failed update saves nothing")
}

func TestPendingLoginIsSingleUseAndExpires(t *testing.T) {
	isolateConfig(t)
	store, err := LoadCredentials()
	require.NoError(t, err)
	now := time.Now()
	require.NoError(t, store.PutPending(PendingLogin{Server: "http://a", State: "s", Verifier: "v", ExpiresAt: now.Add(PendingLoginTTL)}, now))
	require.NoError(t, store.PutPending(PendingLogin{Server: "http://b", State: "s2", Verifier: "v2", ExpiresAt: now.Add(time.Second)}, now))
	require.NoError(t, store.Save())

	loaded, err := LoadCredentials()
	require.NoError(t, err)
	p, ok := loaded.TakePending("http://a/", now)
	require.True(t, ok)
	require.Equal(t, "v", p.Verifier)
	_, ok = loaded.TakePending("http://a", now)
	require.False(t, ok, "single use")
	_, ok = loaded.TakePending("http://b", now.Add(2*time.Second))
	require.False(t, ok, "expired")
}

func newResolverCommand(server string) (*cobra.Command, *bytes.Buffer) {
	cmd := &cobra.Command{Use: "probe"}
	cmd.Flags().String("server", server, "")
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	return cmd, &stderr
}

func TestResolveAPIKeyPrecedence(t *testing.T) {
	isolateConfig(t)
	store, err := LoadCredentials()
	require.NoError(t, err)
	require.NoError(t, store.Put(StoredCredential{Server: "http://srv:8080", Key: "stored-key", ExpiresAt: time.Now().Add(time.Hour)}))
	require.NoError(t, store.Put(StoredCredential{Server: "http://old:8080", Key: "old-key", ExpiresAt: time.Now().Add(-time.Minute)}))
	require.NoError(t, store.Save())

	cmd, stderr := newResolverCommand("http://srv:8080/")
	require.Equal(t, "stored-key", ResolveAPIKey(cmd, "", APIKeyEnvVar), "the stored login is the last resort")

	t.Setenv(APIKeyEnvVar, "env-key")
	require.Equal(t, "env-key", ResolveAPIKey(cmd, "", APIKeyEnvVar), "the env var beats the stored login")
	require.Equal(t, "flag-key", ResolveAPIKey(cmd, "flag-key", APIKeyEnvVar), "the flag beats the env var")
	require.Contains(t, stderr.String(), "visible in process listings")

	t.Setenv(APIKeyEnvVar, "")
	other, _ := newResolverCommand("http://elsewhere:8080")
	require.Empty(t, ResolveAPIKey(other, "", APIKeyEnvVar), "a login is only for its own server")

	expired, expiredErr := newResolverCommand("http://old:8080")
	require.Empty(t, ResolveAPIKey(expired, "", APIKeyEnvVar), "an expired login is not sent")
	require.Contains(t, expiredErr.String(), "run caesium login")
	require.NotContains(t, expiredErr.String(), "old-key", "the key is never printed")

	bare := &cobra.Command{Use: "bare"}
	require.Empty(t, ResolveAPIKey(bare, "", APIKeyEnvVar), "no --server, no stored login")

	headers := make(http.Header)
	SetAuthorization(cmd, headers, "")
	require.Equal(t, "Bearer stored-key", headers.Get("Authorization"))
	empty := make(http.Header)
	SetAuthorization(other, empty, "")
	require.Empty(t, empty.Get("Authorization"))
}

// The resolver and the store key a login by the same CanonicalServer, so a
// login saved under one spelling of an escaped-path server is found under
// another, and a path that differs only by an escape is a different server.
func TestResolveAPIKeyMatchesEscapedServerPaths(t *testing.T) {
	isolateConfig(t)
	store, err := LoadCredentials()
	require.NoError(t, err)
	require.NoError(t, store.Put(StoredCredential{Server: "HTTPS://Host.Example:443/team%23blue/", Key: "team-key", ExpiresAt: time.Now().Add(time.Hour)}))
	require.NoError(t, store.Save())

	cmd, _ := newResolverCommand("https://host.example/team%23blue")
	require.Equal(t, "team-key", ResolveAPIKey(cmd, "", APIKeyEnvVar))
	for _, other := range []string{"https://host.example/team", "https://host.example/team%2Fblue", "https://host.example/team%23blue/x"} {
		c, _ := newResolverCommand(other)
		require.Empty(t, ResolveAPIKey(c, "", APIKeyEnvVar), other)
	}
	reloaded, err := LoadCredentials()
	require.NoError(t, err)
	cred, ok := reloaded.Lookup("https://host.example:443/team%23blue")
	require.True(t, ok)
	require.Equal(t, "https://host.example/team%23blue", cred.Server)
}

func TestResolveAPIKeyIgnoresAnUnsafeCredentialsFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	xdg := isolateConfig(t)
	store, err := LoadCredentials()
	require.NoError(t, err)
	require.NoError(t, store.Put(StoredCredential{Server: "http://srv:8080", Key: "stored-key"}))
	require.NoError(t, store.Save())
	require.NoError(t, os.Chmod(filepath.Join(xdg, "caesium", "credentials.yaml"), 0o640))

	cmd, stderr := newResolverCommand("http://srv:8080")
	require.Empty(t, ResolveAPIKey(cmd, "", APIKeyEnvVar))
	require.Contains(t, stderr.String(), "ignoring stored credentials")
}

func TestServerFlagReadsInheritedFlags(t *testing.T) {
	parent := &cobra.Command{Use: "parent"}
	parent.PersistentFlags().String("server", "http://inherited:1", "")
	child := &cobra.Command{Use: "child", Run: func(*cobra.Command, []string) {}}
	parent.AddCommand(child)
	require.Equal(t, "http://inherited:1", ServerFlag(child))
	require.Empty(t, ServerFlag(nil))
}
