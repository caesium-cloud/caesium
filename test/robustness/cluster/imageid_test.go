//go:build !integration

package cluster

import "testing"

func TestImageIDMatchesCandidateMapsManifestAndConfig(t *testing.T) {
	config := "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	manifest := "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	candidate := config + "," + manifest
	if !ImageIDMatchesCandidate("containerd://"+config, candidate) {
		t.Fatalf("config imageID should match candidate set")
	}
	if !ImageIDMatchesCandidate("docker-pullable://caesiumcloud/caesium@"+manifest, candidate) {
		t.Fatalf("manifest imageID should match candidate set")
	}
	other := "sha256:" + "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	if ImageIDMatchesCandidate(other, candidate) {
		t.Fatalf("unrelated digest must not match")
	}
	if ImageIDMatchesCandidate("containerd://"+config, "") {
		t.Fatalf("empty candidate must not match")
	}
}

func TestNormalizeImageDigest(t *testing.T) {
	got := NormalizeImageDigest("docker-pullable://x@sha256:ABCD" + "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	want := "sha256:abcd" + "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	if got != want {
		t.Fatalf("NormalizeImageDigest=%q want %q", got, want)
	}
}
