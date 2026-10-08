//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func requestPacket(payload string) []byte {
	return []byte(fmt.Sprintf("%04x%s", len(payload)+4, payload))
}

func TestRequestFencesServiceRepositoryHostAndLength(t *testing.T) {
	const source = "git://owned-fixture:9418/coverage.git"
	for _, host := range []string{"owned-fixture", "owned-fixture:9418"} {
		if err := readRequest(bytes.NewReader(requestPacket("git-upload-pack /coverage.git\x00host="+host+"\x00")), source); err != nil {
			t.Fatal(err)
		}
	}
	for name, packet := range map[string][]byte{
		"receive-pack": requestPacket("git-receive-pack /coverage.git\x00host=owned-fixture\x00"),
		"traversal":    requestPacket("git-upload-pack /../coverage.git\x00host=owned-fixture\x00"),
		"foreign repo": requestPacket("git-upload-pack /foreign.git\x00host=owned-fixture\x00"),
		"foreign host": requestPacket("git-upload-pack /coverage.git\x00host=foreign\x00"),
		"extra argv":   requestPacket("git-upload-pack --exec=sh /coverage.git\x00host=owned-fixture\x00"),
		"capability":   requestPacket("git-upload-pack /coverage.git\x00host=owned-fixture\x00\x00version=2\x00"),
		"missing NUL":  requestPacket("git-upload-pack /coverage.git\x00host=owned-fixture"),
		"truncated":    []byte("0020short"),
		"oversize":     []byte("2001"),
		"flush":        []byte("0000"),
		"invalid hex":  []byte("zzzz"),
	} {
		t.Run(name, func(t *testing.T) {
			if err := readRequest(bytes.NewReader(packet), source); err == nil {
				t.Fatal("outside request was admitted")
			}
		})
	}
}

func TestFixtureStateRefusesForeignIdentityAndIncompleteBytes(t *testing.T) {
	state := fixtureState{SchemaVersion: 1, Alias: "coverage-owned", SourceID: "coverage-owned",
		URL: "git://owned-fixture:9418/coverage.git", Ref: "main", Path: manifestPath, Image: "alpine:3.23"}
	if err := validateState(state); err != nil {
		t.Fatal(err)
	}
	for _, foreign := range []string{"file:///tmp/coverage.git", "git://owned-fixture:9418/foreign.git",
		"git://user@owned-fixture:9418/coverage.git", "git://owned-fixture:9418/coverage.git?exec=sh",
		"git://owned-fixture:9418/%63overage.git", "git://owned-fixture:9418/coverage.git#extra"} {
		invalid := state
		invalid.URL = foreign
		if err := validateState(invalid); err == nil {
			t.Fatalf("accepted foreign source %q", foreign)
		}
	}
	path := filepath.Join(t.TempDir(), "state.json")
	for _, data := range []string{`{"schema_version":1`, strings.Repeat(" ", 16385)} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readState(path); err == nil {
			t.Fatal("incomplete or oversized state admitted")
		}
	}
}

func TestFixturePathRefusesSymlinksAndForeignNames(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "coverage.git")
	if err := validateRepoPath(repo); err != nil {
		t.Fatal(err)
	}
	if err := validateRepoPath(filepath.Join(root, "foreign.git")); err == nil {
		t.Fatal("accepted foreign repository name")
	}
	if err := os.Symlink(root, repo); err != nil {
		t.Fatal(err)
	}
	if err := validateRepoPath(repo); err == nil {
		t.Fatal("accepted substituted repository")
	}
	if err := initialize(context.Background(), repo, fixtureState{}); err == nil {
		t.Fatal("accepted initialization over a foreign path")
	}
}

// Container qualification runs this against the already-loaded full builder's
// real Git. This assertion inspects native ref advertisement, not synthetic refs.
func TestNativeGitCreatesAndAdvertisesActualCommittedManifest(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "coverage.git")
	state := fixtureState{SchemaVersion: 1, Alias: "coverage-owned", SourceID: "coverage-owned",
		URL: "git://owned-fixture:9418/coverage.git", Ref: "main", Path: manifestPath, Image: "alpine:3.23"}
	if err := initialize(t.Context(), repo, state); err != nil {
		t.Fatal(err)
	}
	saved, err := readState(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateRepository(t.Context(), repo, saved); err != nil {
		t.Fatal(err)
	}
	advertisement, err := nativeGit(t.Context(), repo, "upload-pack", "--strict", "--advertise-refs", filepath.Join(repo, ".git"))
	if err != nil || !strings.Contains(advertisement, saved.InitialCommit+" refs/heads/main") {
		t.Fatal("native upload-pack did not advertise actual committed main")
	}
	manifest, err := nativeGit(t.Context(), repo, "show", saved.InitialCommit+":"+manifestPath)
	if err != nil || !strings.Contains(manifest, "marker") || !strings.Contains(manifest, "initial") {
		t.Fatal("native commit does not contain the actual initial definition")
	}
	if err := initialize(t.Context(), repo, state); err == nil {
		t.Fatal("second initialization overwrote repository")
	}
	head, err := nativeGit(t.Context(), repo, "rev-parse", "HEAD")
	if err != nil || head != saved.InitialCommit {
		t.Fatal("refusal changed the initial repository")
	}
}

func TestNativeUploadPackWrapperAdvertisesActualHeadAndJoins(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "coverage.git")
	state := fixtureState{SchemaVersion: 1, Alias: "coverage-owned", SourceID: "coverage-owned",
		URL: "git://owned-fixture:9418/coverage.git", Ref: "main", Path: manifestPath, Image: "alpine:3.23"}
	if err := initialize(t.Context(), repo, state); err != nil {
		t.Fatal(err)
	}
	saved, err := readState(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	// net.Pipe is in-memory: no daemon or TCP/network resource is allocated.
	server, client := net.Pipe()
	defer client.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	joined := make(chan error, 1)
	go func() { joined <- uploadPack(ctx, server, repo, state.URL) }()
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(requestPacket("git-upload-pack /coverage.git\x00host=owned-fixture\x00")); err != nil {
		t.Fatal(err)
	}
	var advertised strings.Builder
	for advertised.Len() < 16384 {
		var header [4]byte
		if _, err := io.ReadFull(client, header[:]); err != nil {
			t.Fatal("native advertisement failed:", err)
		}
		length, err := strconv.ParseUint(string(header[:]), 16, 16)
		if err != nil || (length != 0 && length < 4) {
			t.Fatal("native advertisement is not pkt-line")
		}
		if length == 0 {
			break
		}
		data := make([]byte, int(length)-4)
		if _, err := io.ReadFull(client, data); err != nil {
			t.Fatal(err)
		}
		advertised.Write(data)
	}
	if !strings.Contains(advertised.String(), saved.InitialCommit+" refs/heads/main") {
		t.Fatal("wrapper did not serve native committed main")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("wrapper did not join its upload-pack child after cancellation")
	}
}
