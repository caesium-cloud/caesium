//go:build integration

package ldap

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	authpkg "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
	cerrdefs "github.com/containerd/errdefs"
	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/stretchr/testify/require"
)

const (
	openLDAPImage      = "osixia/openldap:1.5.0"
	openLDAPLDIFPath   = "testdata/openldap/bootstrap.ldif"
	openLDAPAdminDN    = "cn=admin,dc=example,dc=org"
	openLDAPAdminPass  = "admin-secret"
	openLDAPUserDN     = "uid=alice,ou=people,dc=example,dc=org"
	openLDAPUser       = "alice"
	openLDAPUserPass   = "alice-secret"
	openLDAPGroupBase  = "ou=groups,dc=example,dc=org"
	openLDAPPeopleBase = "ou=people,dc=example,dc=org"
)

var openLDAPLDAPS = nat.Port("636/tcp")

func TestProviderAuthenticateOpenLDAPFixture(t *testing.T) {
	cli := dockerClientOrSkip(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	ensureOpenLDAPImage(t, ctx, cli)
	containerID := createOpenLDAPContainer(t, ctx, cli)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		_ = cli.ContainerRemove(cleanupCtx, containerID, dockercontainer.RemoveOptions{
			Force:         true,
			RemoveVolumes: true,
		})
	})

	require.NoError(t, cli.ContainerStart(ctx, containerID, dockercontainer.StartOptions{}))
	endpoints := openLDAPEndpoints(t, ctx, cli, containerID)
	identity := waitForOpenLDAPIdentity(t, ctx, cli, containerID, endpoints)

	require.Equal(t, ProviderName, identity.Issuer)
	require.Equal(t, openLDAPUserDN, identity.Subject)
	require.Equal(t, "alice@example.org", identity.Email)
	require.Equal(t, "Alice Fixture", identity.DisplayName)

	viewersGroupDN := "cn=caesium-viewers," + openLDAPGroupBase
	operatorsGroupDN := "cn=caesium-operators," + openLDAPGroupBase
	require.ElementsMatch(t, []string{viewersGroupDN, operatorsGroupDN}, identity.Groups)

	mapper, err := authpkg.NewRoleMapper(
		viewersGroupDN+"=viewer;"+operatorsGroupDN+"=operator",
		"",
	)
	require.NoError(t, err)
	role, ok := mapper.Resolve(identity.Groups)
	require.True(t, ok)
	require.Equal(t, models.RoleOperator, role)
}

func dockerClientOrSkip(t *testing.T) *client.Client {
	t.Helper()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Skipf("Docker unavailable for OpenLDAP fixture: %v", err)
	}
	t.Cleanup(func() {
		_ = cli.Close()
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := cli.Ping(ctx); err != nil {
		t.Skipf("Docker unavailable for OpenLDAP fixture: %v", err)
	}
	return cli
}

func ensureOpenLDAPImage(t *testing.T, ctx context.Context, cli *client.Client) {
	t.Helper()

	if _, err := cli.ImageInspect(ctx, openLDAPImage); err == nil {
		return
	} else if !cerrdefs.IsNotFound(err) {
		require.NoError(t, err)
	}

	pulled, err := cli.ImagePull(ctx, openLDAPImage, image.PullOptions{})
	require.NoError(t, err, "pull %s", openLDAPImage)
	defer func() {
		_ = pulled.Close()
	}()
	_, err = io.Copy(io.Discard, pulled)
	require.NoError(t, err, "read %s pull stream", openLDAPImage)
}

func createOpenLDAPContainer(t *testing.T, ctx context.Context, cli *client.Client) string {
	t.Helper()

	ldifArchive := openLDAPLDIFArchive(t)
	created, err := cli.ContainerCreate(
		ctx,
		&dockercontainer.Config{
			Image: openLDAPImage,
			Env: []string{
				"LDAP_ORGANISATION=Caesium",
				"LDAP_DOMAIN=example.org",
				"LDAP_ADMIN_PASSWORD=" + openLDAPAdminPass,
				"LDAP_TLS=true",
				"LDAP_TLS_VERIFY_CLIENT=never",
			},
			ExposedPorts: nat.PortSet{openLDAPLDAPS: struct{}{}},
		},
		&dockercontainer.HostConfig{
			PortBindings: nat.PortMap{
				openLDAPLDAPS: []nat.PortBinding{{HostIP: "127.0.0.1"}},
			},
		},
		nil,
		nil,
		"",
	)
	require.NoError(t, err)

	err = cli.CopyToContainer(
		ctx,
		created.ID,
		"/",
		ldifArchive,
		dockercontainer.CopyToContainerOptions{},
	)
	require.NoError(t, err)
	return created.ID
}

func openLDAPLDIFArchive(t *testing.T) io.Reader {
	t.Helper()

	ldif, err := os.ReadFile(openLDAPLDIFPath)
	require.NoError(t, err)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, dir := range []string{
		"container",
		"container/service",
		"container/service/slapd",
		"container/service/slapd/assets",
		"container/service/slapd/assets/config",
		"container/service/slapd/assets/config/bootstrap",
		"container/service/slapd/assets/config/bootstrap/ldif",
		"container/service/slapd/assets/config/bootstrap/ldif/custom",
	} {
		err = tw.WriteHeader(&tar.Header{
			Name:     dir + "/",
			Mode:     0o755,
			Typeflag: tar.TypeDir,
		})
		require.NoError(t, err)
	}

	err = tw.WriteHeader(&tar.Header{
		Name: "container/service/slapd/assets/config/bootstrap/ldif/custom/50-caesium-users.ldif",
		Mode: 0o644,
		Size: int64(len(ldif)),
	})
	require.NoError(t, err)
	_, err = tw.Write(ldif)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	return bytes.NewReader(buf.Bytes())
}

func openLDAPEndpoints(t *testing.T, ctx context.Context, cli *client.Client, containerID string) []string {
	t.Helper()

	inspect, err := cli.ContainerInspect(ctx, containerID)
	require.NoError(t, err)

	var endpoints []string
	for _, binding := range inspect.NetworkSettings.Ports[openLDAPLDAPS] {
		if binding.HostPort == "" {
			continue
		}
		host := binding.HostIP
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		endpoints = append(endpoints, net.JoinHostPort(host, binding.HostPort))
		endpoints = append(endpoints, net.JoinHostPort("localhost", binding.HostPort))
		endpoints = append(endpoints, net.JoinHostPort("host.docker.internal", binding.HostPort))
	}

	for _, network := range inspect.NetworkSettings.Networks {
		if network == nil || network.IPAddress == "" {
			continue
		}
		endpoints = append(endpoints, net.JoinHostPort(network.IPAddress, "636"))
	}

	endpoints = compactStrings(endpoints)
	require.NotEmpty(t, endpoints, "OpenLDAP container has no reachable LDAPS endpoints")
	return endpoints
}

func waitForOpenLDAPIdentity(t *testing.T, ctx context.Context, cli *client.Client, containerID string, endpoints []string) *authpkg.ExternalIdentity {
	t.Helper()

	deadline := time.Now().Add(45 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		for _, endpoint := range endpoints {
			if err := probeTCP(ctx, endpoint); err != nil {
				lastErr = err
				continue
			}

			cfg := openLDAPProviderConfig(endpoint)
			provider, err := New(cfg)
			require.NoError(t, err)
			identity, err := provider.Authenticate(ctx, openLDAPUser, openLDAPUserPass)
			if err == nil {
				return identity
			}
			lastErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}

	logs := openLDAPLogs(t, ctx, cli, containerID)
	require.NoErrorf(t, lastErr, "OpenLDAP fixture did not become ready; endpoints=%s\nlogs:\n%s", strings.Join(endpoints, ", "), logs)
	return nil
}

func openLDAPProviderConfig(endpoint string) Config {
	return Config{
		URL:                  "ldaps://" + endpoint,
		TLSConfig:            &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // fixture cert is generated per container for a dynamic endpoint.
		Timeout:              time.Second,
		BindDN:               openLDAPAdminDN,
		BindPassword:         openLDAPAdminPass,
		UserBaseDN:           openLDAPPeopleBase,
		UserFilter:           "(uid={username})",
		GroupBaseDN:          openLDAPGroupBase,
		GroupFilter:          "(member={dn})",
		GroupAttribute:       "dn",
		UsernameAttribute:    DefaultUsernameAttribute,
		EmailAttribute:       DefaultEmailAttribute,
		DisplayNameAttribute: DefaultDisplayNameAttribute,
	}
}

func probeTCP(ctx context.Context, endpoint string) error {
	probeCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(probeCtx, "tcp", endpoint)
	if err != nil {
		return fmt.Errorf("dial %s: %w", endpoint, err)
	}
	return conn.Close()
}

func openLDAPLogs(t *testing.T, ctx context.Context, cli *client.Client, containerID string) string {
	t.Helper()

	logs, err := cli.ContainerLogs(ctx, containerID, dockercontainer.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       "120",
	})
	if err != nil {
		return "failed to read logs: " + err.Error()
	}
	defer func() {
		_ = logs.Close()
	}()
	out, err := io.ReadAll(logs)
	if err != nil {
		return "failed to read logs: " + err.Error()
	}
	return string(out)
}

func compactStrings(values []string) []string {
	out := values[:0]
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func TestOpenLDAPLDIFArchiveIncludesFixture(t *testing.T) {
	archive := openLDAPLDIFArchive(t)
	tr := tar.NewReader(archive)

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if filepath.Base(hdr.Name) != "50-caesium-users.ldif" {
			continue
		}
		body, err := io.ReadAll(tr)
		require.NoError(t, err)
		require.Contains(t, string(body), "uid=alice,ou=people,dc=example,dc=org")
		return
	}
	t.Fatal("fixture LDIF missing from archive")
}
