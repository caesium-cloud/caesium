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
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
	gldap "github.com/go-ldap/ldap/v3"
	"github.com/google/uuid"
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
	identity, provider, endpoint := waitForOpenLDAPIdentity(t, ctx, cli, containerID, endpoints)

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
	grants := mapper.Resolve(identity.Groups, "")
	require.NotEmpty(t, grants)
	require.Equal(t, models.RoleOperator, grants.ClusterRole())

	// E1: the login's grant is the user DN, and a refresh re-queries the
	// directory by DN with the service account alone.
	require.NotNil(t, identity.Refresh)
	require.Equal(t, openLDAPUserDN, identity.Refresh.Handle.Reveal())
	refreshReq := authpkg.RefreshRequest{
		UserID:  uuid.New(),
		Issuer:  identity.Issuer,
		Subject: identity.Subject,
		Grant:   *identity.Refresh,
	}
	refreshed, err := provider.RefreshGroups(ctx, refreshReq)
	require.NoError(t, err)
	require.True(t, refreshed.GroupsKnown)
	require.ElementsMatch(t, identity.Groups, refreshed.Groups)
	require.Equal(t, openLDAPUserDN, refreshed.Handle.Reveal())

	// A membership change in the directory shows up on the next refresh.
	admin := openLDAPAdmin(t, endpoint)
	removeMember := gldap.NewModifyRequest(operatorsGroupDN, nil)
	// groupOfNames needs a member, so swap alice for another DN.
	removeMember.Replace("member", []string{openLDAPAdminDN})
	require.NoError(t, admin.Modify(removeMember))
	refreshed, err = provider.RefreshGroups(ctx, refreshReq)
	require.NoError(t, err)
	require.Equal(t, []string{viewersGroupDN}, refreshed.Groups)

	// A refresh re-applies the login filter, rendered with what the user
	// typed. Sign in by mail (the username attribute stays uid) through a
	// filter that excludes disabled accounts: the refresh succeeds while the
	// filter admits the entry, and is a definitive denial once it does not.
	gatedCfg := openLDAPProviderConfig(endpoint)
	gatedCfg.UserFilter = "(&(mail={username})(!(description=disabled)))"
	gated, err := New(gatedCfg)
	require.NoError(t, err)
	gatedIdentity, err := gated.Authenticate(ctx, "alice@example.org", openLDAPUserPass)
	require.NoError(t, err)
	require.Equal(t, "alice@example.org", gatedIdentity.Refresh.Login)
	gatedReq := authpkg.RefreshRequest{
		UserID:  refreshReq.UserID,
		Issuer:  gatedIdentity.Issuer,
		Subject: gatedIdentity.Subject,
		Grant:   *gatedIdentity.Refresh,
	}
	refreshed, err = gated.RefreshGroups(ctx, gatedReq)
	require.NoError(t, err, "a mail login filter with a uid username attribute must not revoke")
	require.Equal(t, []string{viewersGroupDN}, refreshed.Groups)

	disable := gldap.NewModifyRequest(openLDAPUserDN, nil)
	disable.Add("description", []string{"disabled"})
	require.NoError(t, admin.Modify(disable))
	_, err = gated.RefreshGroups(ctx, gatedReq)
	requireOpenLDAPDenial(t, err, "user_not_admitted")
	_, err = gated.Authenticate(ctx, "alice@example.org", openLDAPUserPass)
	require.Error(t, err, "login agrees: the filter no longer admits alice")
	// The ungated provider's filter does not mention description.
	_, err = provider.RefreshGroups(ctx, refreshReq)
	require.NoError(t, err)

	// A base narrowed so it no longer contains the user denies as well.
	narrowedCfg := openLDAPProviderConfig(endpoint)
	narrowedCfg.UserBaseDN = openLDAPGroupBase
	narrowed, err := New(narrowedCfg)
	require.NoError(t, err)
	_, err = narrowed.RefreshGroups(ctx, refreshReq)
	requireOpenLDAPDenial(t, err, "user_outside_base")

	// An entry that does not exist is a definitive denial of the subject.
	for _, ghost := range []string{
		"uid=ghost," + openLDAPPeopleBase,
		"uid=ghost,ou=nowhere,dc=example,dc=org",
	} {
		ghostReq := refreshReq
		ghostReq.Subject = ghost
		ghostReq.Grant.Handle = authpkg.NewRefreshHandle(ghost)
		_, err = provider.RefreshGroups(ctx, ghostReq)
		refreshErr, ok := err.(*authpkg.RefreshError)
		require.Truef(t, ok, "want *RefreshError for %s, got %T: %v", ghost, err, err)
		require.Equal(t, authpkg.RefreshDefinitive, refreshErr.Kind)
		require.Equal(t, authpkg.DenialOfSubject, refreshErr.Scope)
	}

	// Deleting the user's entry turns the user's own refresh definitive.
	require.NoError(t, admin.Del(gldap.NewDelRequest(openLDAPUserDN, nil)))
	_, err = provider.RefreshGroups(ctx, refreshReq)
	refreshErr, ok := err.(*authpkg.RefreshError)
	require.Truef(t, ok, "want *RefreshError, got %T: %v", err, err)
	require.Equal(t, authpkg.RefreshDefinitive, refreshErr.Kind)
	require.Equal(t, "user_not_found", refreshErr.Reason)
}

func requireOpenLDAPDenial(t *testing.T, err error, reason string) {
	t.Helper()
	refreshErr, ok := err.(*authpkg.RefreshError)
	require.Truef(t, ok, "want *RefreshError, got %T: %v", err, err)
	require.Equal(t, authpkg.RefreshDefinitive, refreshErr.Kind)
	require.Equal(t, authpkg.DenialOfSubject, refreshErr.Scope)
	require.Equal(t, reason, refreshErr.Reason)
}

func openLDAPAdmin(t *testing.T, endpoint string) *gldap.Conn {
	t.Helper()
	conn, err := gldap.DialURL("ldaps://"+endpoint,
		gldap.DialWithTLSConfig(&tls.Config{InsecureSkipVerify: true})) //nolint:gosec // fixture cert is generated per container.
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.Bind(openLDAPAdminDN, openLDAPAdminPass))
	return conn
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

func waitForOpenLDAPIdentity(t *testing.T, ctx context.Context, cli *client.Client, containerID string, endpoints []string) (*authpkg.ExternalIdentity, *Provider, string) {
	t.Helper()

	deadline := time.Now().Add(45 * time.Second)
	var lastErr error
OuterLoop:
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			lastErr = ctx.Err()
			break OuterLoop
		default:
		}

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
				return identity, provider, endpoint
			}
			lastErr = err
		}

		select {
		case <-ctx.Done():
			lastErr = ctx.Err()
			break OuterLoop
		case <-time.After(500 * time.Millisecond):
		}
	}

	logs := openLDAPLogs(t, ctx, cli, containerID)
	require.NoErrorf(t, lastErr, "OpenLDAP fixture did not become ready; endpoints=%s\nlogs:\n%s", strings.Join(endpoints, ", "), logs)
	return nil, nil, ""
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
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, logs); err != nil {
		return "failed to demultiplex logs: " + err.Error()
	}
	return stdout.String() + stderr.String()
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
