repo := "caesiumcloud"
image := "caesium"
builder_image := image + "-builder"
tag := "latest"
dockerfile := "build/Dockerfile"

# Auto-detect host architecture for supported targets (amd64/arm64).
# For other architectures, set CAESIUM_PLATFORM explicitly to a supported value.

docker_arch := if arch() == "aarch64" { "arm64" } else if arch() == "x86_64" { "amd64" } else { arch() }
platform := env("CAESIUM_PLATFORM", "linux/" + docker_arch)
target_arch := if platform == "linux/arm64" { "arm64" } else if platform == "linux/amd64" { "amd64" } else { docker_arch }
bld_dir := "/bld/caesium"
repo_dir := `pwd`
it_container := "caesium-server-test"
uid := `id -u`

# Podman support: set CAESIUM_PODMAN=true to use podman and localhost-prefixed local image refs.

podman := env("CAESIUM_PODMAN", "false")
container_cli := if podman == "true" { env("CAESIUM_CONTAINER_CLI", "podman") } else { env("CAESIUM_CONTAINER_CLI", "docker") }
xdg_runtime_dir := env("XDG_RUNTIME_DIR", "")
podman_sock := if xdg_runtime_dir != "" { xdg_runtime_dir + "/podman/podman.sock" } else { "/run/user/" + uid + "/podman/podman.sock" }
default_sock := if podman == "true" { podman_sock } else { "/var/run/docker.sock" }
local_image_ref := if podman == "true" { "localhost/" + repo + "/" + image } else { repo + "/" + image }
local_builder_ref := if podman == "true" { "localhost/" + repo + "/" + builder_image } else { repo + "/" + builder_image }
publish_image_ref := repo + "/" + image
publish_builder_ref := repo + "/" + builder_image
sock := env("CAESIUM_SOCK", default_sock)
port := env("CAESIUM_PORT", "8080")
auth_mode := env("CAESIUM_AUTH_MODE", "none")

# Local Docker registry used by `just k8s-distributed` to push freshly-built
# images into the cluster's containerd. Port 5050 sidesteps the macOS
# AirPlay Receiver, which binds 5000 by default.
k8s_registry_port := env("CAESIUM_K8S_REGISTRY_PORT", "5050")
k8s_registry_name := "caesium-dev-registry"

validate-platform:
    @if [ "{{ platform }}" != "linux/amd64" ] && [ "{{ platform }}" != "linux/arm64" ]; then \
      echo "Unsupported CAESIUM_PLATFORM '{{ platform }}' (supported: linux/amd64, linux/arm64)"; \
      exit 1; \
    fi

builder: validate-platform
    @if {{ container_cli }} image inspect {{ local_builder_ref }}:{{ tag }} >/dev/null 2>&1; then \
        echo "Builder image {{ local_builder_ref }}:{{ tag }} already exists, skipping build."; \
    else \
        {{ container_cli }} build --platform {{ platform }} \
            --build-arg TARGETARCH={{ target_arch }} \
            --target builder \
            -t {{ local_builder_ref }}:{{ tag }} \
            -f {{ dockerfile }}.build .; \
    fi

builder-full: validate-platform
    @if {{ container_cli }} image inspect {{ local_builder_ref }}:{{ tag }}-full >/dev/null 2>&1; then \
        echo "Builder image {{ local_builder_ref }}:{{ tag }}-full already exists, skipping build."; \
    else \
        {{ container_cli }} build --platform {{ platform }} \
            --build-arg TARGETARCH={{ target_arch }} \
            --target builder-full \
            -t {{ local_builder_ref }}:{{ tag }}-full \
            -f {{ dockerfile }}.build .; \
    fi

build: builder
    {{ container_cli }} build --platform {{ platform }} \
        --build-arg BUILDER_IMAGE={{ local_builder_ref }}:{{ tag }} \
        --target release \
        -t {{ local_image_ref }}:{{ tag }} \
        -f {{ dockerfile }} .

# Build for a specific platform (requires buildx + QEMU for cross-platform)
build-cross target_platform:
    docker buildx build --platform {{ target_platform }} \
        --target builder \
        -t {{ publish_builder_ref }}:{{ tag }} \
        -f {{ dockerfile }}.build --load .
    docker buildx build --platform {{ target_platform }} \
        --build-arg BUILDER_IMAGE={{ publish_builder_ref }}:{{ tag }} \
        --target release \
        -t {{ publish_image_ref }}:{{ tag }} \
        -f {{ dockerfile }} --load .

# Build and push multi-arch images for both builder and runtime
build-multiarch:
    docker buildx build --platform linux/amd64,linux/arm64 \
        --target builder \
        -t {{ publish_builder_ref }}:{{ tag }} \
        -f {{ dockerfile }}.build --push .
    docker buildx build --platform linux/amd64,linux/arm64 \
        --build-arg BUILDER_IMAGE={{ publish_builder_ref }}:{{ tag }} \
        --target release \
        -t {{ publish_image_ref }}:{{ tag }} \
        -f {{ dockerfile }} --push .

build-release: builder
    {{ container_cli }} build --platform {{ platform }} \
        --build-arg BUILDER_IMAGE={{ local_builder_ref }}:{{ tag }} \
        --target release \
        -t {{ local_image_ref }}:{{ tag }} \
        -f {{ dockerfile }} .

build-test: builder
    {{ container_cli }} build --platform {{ platform }} \
        --build-arg BUILDER_IMAGE={{ local_builder_ref }}:{{ tag }} \
        --target test \
        -t {{ local_image_ref }}:{{ tag }}-test \
        -f {{ dockerfile }} .

push:
    docker push {{ publish_image_ref }}:{{ tag }}

push-test:
    docker push {{ publish_image_ref }}:{{ tag }}-test

push-multiarch:
    docker push {{ publish_image_ref }}:{{ tag }}-amd64
    docker push {{ publish_image_ref }}:{{ tag }}-arm64
    docker manifest create {{ publish_image_ref }}:{{ tag }} \
        {{ publish_image_ref }}:{{ tag }}-amd64 \
        {{ publish_image_ref }}:{{ tag }}-arm64
    docker manifest push {{ publish_image_ref }}:{{ tag }}

unit-test: builder-full
    {{ container_cli }} run --rm --platform {{ platform }} \
        -v {{ repo_dir }}:{{ bld_dir }} \
        -w {{ bld_dir }} \
        {{ local_builder_ref }}:{{ tag }}-full \
        sh -c 'mkdir -p ui/dist && touch ui/dist/index.html && go test -race -coverprofile=coverage.txt -covermode=atomic -v ./...'

run: build
    {{ container_cli }} run --platform {{ platform }} \
        -d --name caesium-server \
        -p {{ port }}:8080 \
        -v {{ sock }}:/var/run/docker.sock \
        -e DOCKER_HOST=unix:///var/run/docker.sock \
        -e CAESIUM_AUTH_MODE={{ auth_mode }} \
        -e CAESIUM_AUTH_REQUIRE_TLS=false \
        --user 0:0 \
        {{ local_image_ref }}:{{ tag }} start

rm:
    {{ container_cli }} rm -f caesium-server

integration-test:
    just tag={{ tag }} integration-up
    @cli_dir={{ repo_dir }}/.tmp/caesium-cli; \
    rm -rf "$cli_dir"; \
    mkdir -p "$cli_dir"; \
    cli_ctr=$({{ container_cli }} create --platform {{ platform }} {{ local_image_ref }}:{{ tag }}-test true); \
    trap '{{ container_cli }} rm -f "$cli_ctr" >/dev/null 2>&1 || true; rm -rf "$cli_dir"' EXIT; \
    {{ container_cli }} cp "$cli_ctr":/bin/caesium "$cli_dir/caesium"; \
    chmod +x "$cli_dir/caesium"; \
    {{ container_cli }} rm -f "$cli_ctr" >/dev/null 2>&1 || true; \
    if {{ container_cli }} run --rm --platform {{ platform }} \
        -v {{ repo_dir }}:{{ bld_dir }} \
        -v {{ sock }}:/var/run/docker.sock \
        -e CAESIUM_CLI_PATH={{ bld_dir }}/.tmp/caesium-cli/caesium \
        -e DOCKER_HOST=unix:///var/run/docker.sock \
        --network=container:{{ it_container }} \
        -w {{ bld_dir }} \
        {{ local_builder_ref }}:{{ tag }}-full \
        sh -c 'mkdir -p ui/dist && touch ui/dist/index.html && go test ./test/ -tags=integration'; then \
      {{ container_cli }} rm -f {{ it_container }} >/dev/null 2>&1 || true; \
    else \
      echo "integration tests failed; caesium server logs:"; \
      {{ container_cli }} logs {{ it_container }} || true; \
      {{ container_cli }} rm -f {{ it_container }} >/dev/null 2>&1 || true; \
      exit 1; \
    fi

integration-down:
    {{ container_cli }} rm -f {{ it_container }}

# Run integration tests against a Caesium server using the Podman engine.
# Requires Podman to be installed and the Podman socket to be active

# (run: systemctl --user enable --now podman.socket).
integration-test-podman: build
    #!/usr/bin/env bash
    set -euo pipefail
    PODMAN_SOCK="/run/user/$(id -u)/podman/podman.sock"
    docker rm -f caesium-server-podman >/dev/null 2>&1 || true
    docker run -d --name caesium-server-podman \
        --platform {{ platform }} \
        -p 8080:8080 \
        -v "${PODMAN_SOCK}:/run/podman/podman.sock" \
        -e CAESIUM_PODMAN_URI=unix:///run/podman/podman.sock \
        -e CAESIUM_MANUAL_TRIGGER_API_KEY=integration-test-key \
        -e CAESIUM_LOG_LEVEL=debug \
        --user 0:0 \
        {{ repo }}/{{ image }}:{{ tag }} start
    if docker run --rm --platform {{ platform }} \
        -v {{ repo_dir }}:{{ bld_dir }} \
        -e CAESIUM_TEST_ENGINE=podman \
        --network=host \
        -w {{ bld_dir }} \
        {{ repo }}/{{ builder_image }}:{{ tag }}-full \
        sh -c 'mkdir -p ui/dist && touch ui/dist/index.html && go test ./test/ -tags=integration -timeout 10m'; then
      docker rm -f caesium-server-podman >/dev/null 2>&1 || true
    else
      echo "integration tests (podman) failed; caesium server logs:"
      docker logs caesium-server-podman || true
      docker rm -f caesium-server-podman >/dev/null 2>&1 || true
      exit 1
    fi

hydrate:
    {{ container_cli }} run --platform {{ platform }} \
        --rm \
        --network=host \
        -v {{ repo_dir }}/docs/examples:/examples:ro \
        {{ local_image_ref }}:{{ tag }} job apply --server http://127.0.0.1:{{ port }} --path /examples

integration-up: build-test
    {{ container_cli }} rm -f {{ it_container }} >/dev/null 2>&1 || true
    {{ container_cli }} run -d --platform {{ platform }} \
        --name {{ it_container }} \
        --privileged \
        -v {{ sock }}:/var/run/docker.sock \
        -e DOCKER_HOST=unix:///var/run/docker.sock \
        --user 0:0 \
        -e CAESIUM_MANUAL_TRIGGER_API_KEY=integration-test-key \
        -e CAESIUM_LOG_LEVEL=debug \
        {{ local_image_ref }}:{{ tag }}-test start

lint: builder-full
    {{ container_cli }} run --rm --platform {{ platform }} \
        -v {{ repo_dir }}:{{ bld_dir }} \
        -w {{ bld_dir }} \
        -e GOFLAGS=-buildvcs=false \
        {{ local_builder_ref }}:{{ tag }}-full \
        sh -c 'mkdir -p ui/dist && touch ui/dist/index.html && set -euo pipefail; \
            go fmt .; \
            go vet ./...; \
            golangci-lint run ./...'

ui-lint: builder-full
    {{ container_cli }} run --rm --platform {{ platform }} \
        -v {{ repo_dir }}:{{ bld_dir }} \
        -w {{ bld_dir }}/ui \
        {{ local_builder_ref }}:{{ tag }}-full \
        sh -c 'npm ci --prefer-offline && npm run lint'

ui-test: builder-full
    {{ container_cli }} run --rm --platform {{ platform }} \
        -v {{ repo_dir }}:{{ bld_dir }} \
        -w {{ bld_dir }}/ui \
        {{ local_builder_ref }}:{{ tag }}-full \
        sh -c 'npm ci --prefer-offline && npm test && npm run build:ci'

ui-e2e: build-release
    @{{ container_cli }} rm -f caesium-server >/dev/null 2>&1 || true
    bash -lc 'set -euo pipefail; \
        trap "{{ container_cli }} rm -f caesium-server >/dev/null 2>&1 || true" EXIT; \
        {{ container_cli }} run --platform {{ platform }} -d --name caesium-server -p {{ port }}:8080 \
            -v {{ sock }}:/var/run/docker.sock \
            -e DOCKER_HOST=unix:///var/run/docker.sock \
            -e CAESIUM_MANUAL_TRIGGER_API_KEY=e2e-test-key \
            --user 0:0 {{ local_image_ref }}:{{ tag }} start >/dev/null; \
        tries=0; \
        until curl -sf http://127.0.0.1:{{ port }}/health >/dev/null; do \
            tries=$$((tries + 1)); \
            if [ "$$tries" -gt 30 ]; then \
                echo "Caesium did not become ready in time" >&2; \
                {{ container_cli }} logs caesium-server >&2 || true; \
                exit 1; \
            fi; \
            sleep 1; \
        done; \
        cd ui; \
        npm ci; \
        npx playwright install chromium; \
        npm run test:e2e'

helm-lint:
    helm lint ./helm/caesium

helm-template:
    helm template caesium ./helm/caesium --values ./helm/caesium/ci/test-values.yaml

helm-test:
    helm test caesium --timeout 120s

# Spin up the local dev registry and configure the cluster's containerd to
# pull from it via host.docker.internal. Idempotent. Targets Docker Desktop
# Kubernetes; other clusters (Kind, Minikube) need different wiring.
k8s-registry-up:
    @if ! kubectl cluster-info >/dev/null 2>&1; then echo "Error: Kubernetes cluster not reachable. Ensure Docker Desktop K8s or Kind is running." && exit 1; fi
    @ctx="$(kubectl config current-context)"; \
        if [ "$ctx" != "docker-desktop" ]; then \
            echo "Warning: detected context '$ctx'. This helper is verified on Docker Desktop. Other clusters may need different setup."; \
        fi
    @if ! {{ container_cli }} inspect {{ k8s_registry_name }} >/dev/null 2>&1; then \
        echo "Starting local registry on 127.0.0.1:{{ k8s_registry_port }}..."; \
        {{ container_cli }} run -d --restart=always --name {{ k8s_registry_name }} \
            -p 127.0.0.1:{{ k8s_registry_port }}:5000 registry:2 >/dev/null; \
    elif [ "$({{ container_cli }} inspect -f '{{ "{{.State.Running}}" }}' {{ k8s_registry_name }})" != "true" ]; then \
        echo "Restarting stopped local registry {{ k8s_registry_name }}..."; \
        {{ container_cli }} start {{ k8s_registry_name }} >/dev/null; \
    else \
        echo "Local registry {{ k8s_registry_name }} already running."; \
    fi
    @./scripts/k8s-registry-bypass.sh {{ k8s_registry_port }}

# Tear down the local dev registry (does not affect deployed pods).
k8s-registry-down:
    -{{ container_cli }} rm -f {{ k8s_registry_name }}

# Deploy Caesium in a distributed 3-node Raft cluster on local Kubernetes.
# Tags the image with a unique dev tag, pushes to the local registry, and
# helm-deploys with pullPolicy=Always so each invocation rolls out the freshly
# built bits. See `k8s-registry-up` for the registry/containerd wiring.
k8s-distributed: build-release k8s-registry-up
    @dev_tag="dev-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"; \
        if ! git diff --quiet 2>/dev/null || ! git diff --cached --quiet 2>/dev/null; then \
            dev_tag="${dev_tag}-dirty-$(date +%s)"; \
        fi; \
        echo "Pushing image to local dev registry as ${dev_tag}..."; \
        {{ container_cli }} tag {{ local_image_ref }}:{{ tag }} localhost:{{ k8s_registry_port }}/{{ image }}:${dev_tag}; \
        {{ container_cli }} push localhost:{{ k8s_registry_port }}/{{ image }}:${dev_tag} >/dev/null; \
        helm upgrade --install caesium ./helm/caesium \
            --set replicaCount=3 \
            --set image.repository=host.docker.internal:{{ k8s_registry_port }}/{{ image }} \
            --set image.tag=${dev_tag} \
            --set image.pullPolicy=Always \
            --set config.extraEnv[0].name=CAESIUM_EXECUTION_MODE \
            --set config.extraEnv[0].value=distributed \
            --set kubernetes.engine.enabled=true \
            --set persistence.enabled=false \
            --wait
    @echo "Caesium distributed cluster is ready."

# Stop the local Kubernetes deployment
k8s-down:
    helm uninstall caesium

# Port-forward to the Caesium service (run in background or separate terminal)
k8s-port-forward:
    @echo "Port-forwarding Caesium UI to http://localhost:{{ port }}..."
    kubectl port-forward service/caesium {{ port }}:8080

# View logs for all Caesium pods
k8s-logs:
    kubectl logs -l app.kubernetes.io/name=caesium --all-containers=true -f --tail=100

# Hydrate example jobs into the Kubernetes-hosted Caesium instance.
# Uses examples-k8s/ (engine: kubernetes) so the steps run as pods inside
# the cluster — the docker-engine examples in docs/examples/ would fail
# here because pods don't have access to the host Docker daemon.
k8s-hydrate:
    {{ container_cli }} run --platform {{ platform }} \
        --rm \
        --network=host \
        -v {{ repo_dir }}/docs/examples-k8s:/examples:ro \
        {{ local_image_ref }}:{{ tag }} job apply --server http://host.docker.internal:{{ port }} --path /examples
