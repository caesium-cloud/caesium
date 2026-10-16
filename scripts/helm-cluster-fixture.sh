#!/usr/bin/env bash
# Shared setup from helm-pod-replacement.sh. Scenario drivers retain resource
# ownership and cleanup; these helpers never adopt or remove a cluster.

helm_fixture_create_cluster() {
  local name="$1" image="$2" kubeconfig="$3" existing
  existing="$(kind get clusters)" || return 1
  if printf '%s\n' "$existing" | grep -qxF "$name"; then
    echo "kind cluster $name already exists; refusing to claim it" >&2
    return 1
  fi
  kind create cluster --name "$name" --image "$image" --kubeconfig "$kubeconfig" --wait 180s
}

helm_fixture_load_image() {
  local cluster="$1" image="$2" directory="${3:-${TMPDIR:-/tmp}}" archive platform
  # Docker's multi-platform store may retain an index without all blobs. Load
  # the actual local platform archive, as the existing replacement fixture does.
  platform="$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$image")" || return 1
  archive="$(mktemp "${directory%/}/kind-image.XXXXXX")" || return 1
  if ! docker image save --platform "$platform" --output "$archive" "$image"; then
    rm -f "$archive"
    return 1
  fi
  if ! kind load image-archive --name "$cluster" "$archive"; then
    rm -f "$archive"
    return 1
  fi
  rm -f "$archive"
}

helm_fixture_install_chart() {
  local kubeconfig="$1" namespace="$2" release="$3" chart="$4"
  shift 4
  helm --kubeconfig "$kubeconfig" install "$release" "$chart" --namespace "$namespace" "$@"
}
