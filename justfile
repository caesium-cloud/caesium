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
agent_it_container := "caesium-server-agent-test"
uid := `id -u`

# Podman support: set CAESIUM_PODMAN=true to use podman and localhost-prefixed local image refs.

podman := env("CAESIUM_PODMAN", "false")
container_cli := if podman == "true" { env("CAESIUM_CONTAINER_CLI", "podman") } else { env("CAESIUM_CONTAINER_CLI", "docker") }
xdg_runtime_dir := env("XDG_RUNTIME_DIR", "")
podman_sock := if xdg_runtime_dir != "" { xdg_runtime_dir + "/podman/podman.sock" } else { "/run/user/" + uid + "/podman/podman.sock" }
default_sock := if podman == "true" { podman_sock } else { "/var/run/docker.sock" }
local_image_ref := if podman == "true" { "localhost/" + repo + "/" + image } else { repo + "/" + image }
local_builder_ref := if podman == "true" { "localhost/" + repo + "/" + builder_image } else { repo + "/" + builder_image }
triage_agent_image := repo + "/triage-agent"
local_triage_agent_ref := if podman == "true" { "localhost/" + triage_agent_image } else { triage_agent_image }
publish_image_ref := repo + "/" + image
publish_builder_ref := repo + "/" + builder_image
sock := env("CAESIUM_SOCK", default_sock)
port := env("CAESIUM_PORT", "8080")
auth_mode := env("CAESIUM_AUTH_MODE", "none")
event_ingest_api_key := env("CAESIUM_EVENT_INGEST_API_KEY", "integration-test-key")
agent_integration_run := env("CAESIUM_AGENT_INTEGRATION_RUN", "TestIntegrationTestSuite/TestAgent")
agent_api_external_url := env("CAESIUM_AGENT_API_EXTERNAL_URL", "http://172.17.0.1:" + port)

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

build-triage-agent: validate-platform
    {{ container_cli }} build --platform {{ platform }} \
        -t {{ local_triage_agent_ref }}:{{ tag }} \
        -t {{ triage_agent_image }}:latest \
        -f build/Dockerfile.triage-agent .

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
        -e CAESIUM_EVENT_INGEST_API_KEY={{ event_ingest_api_key }} \
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

integration-test-distributed:
    just tag={{ tag }} integration-up-distributed
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
        -e CAESIUM_EVENT_INGEST_API_KEY={{ event_ingest_api_key }} \
        -e CAESIUM_EXECUTION_MODE=distributed \
        -e DOCKER_HOST=unix:///var/run/docker.sock \
        --network=container:{{ it_container }} \
        -w {{ bld_dir }} \
        {{ local_builder_ref }}:{{ tag }}-full \
        sh -c 'mkdir -p ui/dist && touch ui/dist/index.html && go test ./test/ -tags=integration -run "TestRunConcurrencyStrategies|TestPriorityRunStartSurfacesAndCronDefault" -timeout 10m'; then \
      {{ container_cli }} rm -f {{ it_container }} >/dev/null 2>&1 || true; \
    else \
      echo "distributed integration tests failed; caesium server logs:"; \
      {{ container_cli }} logs {{ it_container }} || true; \
      {{ container_cli }} rm -f {{ it_container }} >/dev/null 2>&1 || true; \
      exit 1; \
    fi

integration-test-agent:
    just tag={{ tag }} integration-up-agent
    @cli_dir={{ repo_dir }}/.tmp/caesium-cli; \
    rm -rf "$cli_dir"; \
    mkdir -p "$cli_dir"; \
    cli_ctr=$({{ container_cli }} create --platform {{ platform }} {{ local_image_ref }}:{{ tag }}-test true); \
    trap '{{ container_cli }} rm -f "$cli_ctr" >/dev/null 2>&1 || true; rm -rf "$cli_dir"' EXIT; \
    {{ container_cli }} cp "$cli_ctr":/bin/caesium "$cli_dir/caesium"; \
    chmod +x "$cli_dir/caesium"; \
    {{ container_cli }} rm -f "$cli_ctr" >/dev/null 2>&1 || true; \
    admin_key=""; \
    tries=0; \
    until admin_key="$({{ container_cli }} logs {{ agent_it_container }} 2>&1 | awk '/csk_/ { for (i = 1; i <= NF; i++) if ($i ~ /^csk_/) { print $i; exit } }' | tr -d '\r')" && [ -n "$admin_key" ]; do \
        tries=$((tries + 1)); \
        if [ "$tries" -gt 30 ]; then \
            echo "Agent auth bootstrap admin key did not appear in server logs"; \
            {{ container_cli }} logs {{ agent_it_container }} || true; \
            {{ container_cli }} rm -f {{ agent_it_container }} >/dev/null 2>&1 || true; \
            exit 1; \
        fi; \
        sleep 1; \
    done; \
    printf '::add-mask::%s\n' "$admin_key"; \
    tries=0; \
    until curl -fsS -H "Authorization: Bearer $admin_key" http://127.0.0.1:{{ port }}/v1/agentprofiles >/dev/null; do \
        tries=$((tries + 1)); \
        if [ "$tries" -gt 30 ]; then \
            echo "Agent auth lane did not become ready or seed the default profile"; \
            {{ container_cli }} logs {{ agent_it_container }} || true; \
            {{ container_cli }} rm -f {{ agent_it_container }} >/dev/null 2>&1 || true; \
            exit 1; \
        fi; \
        sleep 1; \
    done; \
    if {{ container_cli }} run --rm --platform {{ platform }} \
        -v {{ repo_dir }}:{{ bld_dir }} \
        -v {{ sock }}:/var/run/docker.sock \
        -e CAESIUM_CLI_PATH={{ bld_dir }}/.tmp/caesium-cli/caesium \
        -e CAESIUM_MANUAL_TRIGGER_API_KEY=integration-test-key \
        -e CAESIUM_EVENT_INGEST_API_KEY={{ event_ingest_api_key }} \
        -e CAESIUM_AUTH_ADMIN_KEY="$admin_key" \
        -e CAESIUM_API_KEY="$admin_key" \
        -e CAESIUM_AGENT_AUTH_LANE=true \
        -e CAESIUM_TRIAGE_AGENT_IMAGE={{ triage_agent_image }}:latest \
        -e DOCKER_HOST=unix:///var/run/docker.sock \
        --network=container:{{ agent_it_container }} \
        -w {{ bld_dir }} \
        {{ local_builder_ref }}:{{ tag }}-full \
        sh -c 'mkdir -p ui/dist && touch ui/dist/index.html && go test ./test/ -tags=integration -run "{{ agent_integration_run }}" -timeout 10m'; then \
      {{ container_cli }} rm -f {{ agent_it_container }} >/dev/null 2>&1 || true; \
    else \
      echo "agent auth integration tests failed; caesium server logs:"; \
      {{ container_cli }} logs {{ agent_it_container }} || true; \
      {{ container_cli }} rm -f {{ agent_it_container }} >/dev/null 2>&1 || true; \
      exit 1; \
    fi

integration-down:
    {{ container_cli }} rm -f {{ it_container }}

integration-down-agent:
    {{ container_cli }} rm -f {{ agent_it_container }}

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
        -e CAESIUM_EVENT_INGEST_API_KEY={{ event_ingest_api_key }} \
        -e CAESIUM_LOG_LEVEL=debug \
        -e CAESIUM_FRESHNESS_ENABLED=true \
        --user 0:0 \
        {{ repo }}/{{ image }}:{{ tag }} start
    if docker run --rm --platform {{ platform }} \
        -v {{ repo_dir }}:{{ bld_dir }} \
        -e CAESIUM_TEST_ENGINE=podman \
        -e CAESIUM_EVENT_INGEST_API_KEY={{ event_ingest_api_key }} \
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
    # Keep this CI path sharded to exercise the multi-shard database router.
    {{ container_cli }} run -d --platform {{ platform }} \
        --name {{ it_container }} \
        --privileged \
        -v {{ sock }}:/var/run/docker.sock \
        -e DOCKER_HOST=unix:///var/run/docker.sock \
        --user 0:0 \
        -e CAESIUM_MANUAL_TRIGGER_API_KEY=integration-test-key \
        -e CAESIUM_EVENT_INGEST_API_KEY={{ event_ingest_api_key }} \
        -e CAESIUM_LOG_LEVEL=debug \
        -e CAESIUM_DATABASE_SHARDS=4 \
        -e CAESIUM_OPEN_LINEAGE_ENABLED=true \
        -e CAESIUM_OPEN_LINEAGE_TRANSPORT=console \
        -e CAESIUM_FRESHNESS_ENABLED=true \
        -e CAESIUM_NOTIFICATION_WATCHER_INTERVAL=1s \
        -e CAESIUM_RATE_LIMIT_PRUNER_ENABLED=true \
        -e CAESIUM_RATE_LIMIT_PRUNE_INTERVAL=500ms \
        -e CAESIUM_RUN_QUEUE_ENABLED=true \
        -e CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED=true \
        -e CAESIUM_RUN_QUEUE_DEQUEUE_INTERVAL=500ms \
        {{ local_image_ref }}:{{ tag }}-test start

integration-up-distributed: build-test
    {{ container_cli }} rm -f {{ it_container }} >/dev/null 2>&1 || true
    # Keep this CI path sharded to exercise the multi-shard database router.
    {{ container_cli }} run -d --platform {{ platform }} \
        --name {{ it_container }} \
        --privileged \
        -v {{ sock }}:/var/run/docker.sock \
        -e DOCKER_HOST=unix:///var/run/docker.sock \
        --user 0:0 \
        -e CAESIUM_MANUAL_TRIGGER_API_KEY=integration-test-key \
        -e CAESIUM_EVENT_INGEST_API_KEY={{ event_ingest_api_key }} \
        -e CAESIUM_LOG_LEVEL=debug \
        -e CAESIUM_DATABASE_SHARDS=4 \
        -e CAESIUM_OPEN_LINEAGE_ENABLED=true \
        -e CAESIUM_OPEN_LINEAGE_TRANSPORT=console \
        -e CAESIUM_FRESHNESS_ENABLED=true \
        -e CAESIUM_NOTIFICATION_WATCHER_INTERVAL=1s \
        -e CAESIUM_EXECUTION_MODE=distributed \
        -e CAESIUM_NODE_ADDRESS=127.0.0.1:9001 \
        -e CAESIUM_INTERNAL_WAKEUP_TOKEN=integration-distributed-internal-token \
        -e CAESIUM_INTERNAL_PORT=8443 \
        -e CAESIUM_RUN_OWNER_ENABLED=true \
        -e CAESIUM_RUN_LEASE_TTL=30s \
        -e CAESIUM_RUN_OWNER_DISPATCH_INTERVAL=500ms \
        -e CAESIUM_RUN_OWNER_DISPATCH_DEADLINE=5m \
        -e CAESIUM_WORKER_ENABLED=true \
        -e CAESIUM_WORKER_POOL_SIZE=1 \
        -e CAESIUM_WORKER_POLL_INTERVAL=500ms \
        -e CAESIUM_WORKER_RECLAIM_INTERVAL=500ms \
        -e CAESIUM_WORKER_LEASE_TTL=30s \
        -e CAESIUM_RATE_LIMIT_PRUNER_ENABLED=true \
        -e CAESIUM_RATE_LIMIT_PRUNE_INTERVAL=500ms \
        -e CAESIUM_RUN_QUEUE_ENABLED=true \
        -e CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED=true \
        -e CAESIUM_RUN_QUEUE_DEQUEUE_INTERVAL=500ms \
        {{ local_image_ref }}:{{ tag }}-test start

integration-up-agent: build-test build-triage-agent
    {{ container_cli }} rm -f {{ agent_it_container }} >/dev/null 2>&1 || true
    {{ container_cli }} run -d --platform {{ platform }} \
        --name {{ agent_it_container }} \
        --privileged \
        -p {{ port }}:8080 \
        -v {{ sock }}:/var/run/docker.sock \
        -e DOCKER_HOST=unix:///var/run/docker.sock \
        --user 0:0 \
        -e CAESIUM_MANUAL_TRIGGER_API_KEY=integration-test-key \
        -e CAESIUM_EVENT_INGEST_API_KEY={{ event_ingest_api_key }} \
        -e CAESIUM_LOG_LEVEL=debug \
        -e CAESIUM_DATABASE_SHARDS=4 \
        -e CAESIUM_AUTH_MODE=api-key \
        -e CAESIUM_AUTH_KEY_HASH_SECRET=agent-integration-auth-key-hash-secret-000001 \
        -e CAESIUM_AUTH_REQUIRE_TLS=false \
        -e CAESIUM_AGENT_REMEDIATION_ENABLED=true \
        -e CAESIUM_AGENT_DEFAULT_PROFILE=triage-only \
        -e CAESIUM_AGENT_MAX_CONCURRENT_SESSIONS=1 \
        -e CAESIUM_AGENT_SESSION_TIMEOUT=45s \
        -e CAESIUM_AGENT_INCIDENT_COOLDOWN=1s \
        -e CAESIUM_API_EXTERNAL_URL={{ agent_api_external_url }} \
        -e CAESIUM_OPEN_LINEAGE_ENABLED=true \
        -e CAESIUM_OPEN_LINEAGE_TRANSPORT=console \
        -e CAESIUM_FRESHNESS_ENABLED=true \
        -e CAESIUM_NOTIFICATION_WATCHER_INTERVAL=1s \
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
            -e CAESIUM_OPEN_LINEAGE_ENABLED=true \
            -e CAESIUM_OPEN_LINEAGE_TRANSPORT=console \
            -e CAESIUM_FRESHNESS_ENABLED=true \
            -e CAESIUM_RUN_QUEUE_ENABLED=true \
            -e CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED=true \
            -e CAESIUM_RUN_QUEUE_DEQUEUE_INTERVAL=500ms \
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
        npm run test:e2e -- --project=default'

ui-e2e-auth: build-release
    #!/usr/bin/env bash
    set -euo pipefail
    {{ container_cli }} rm -f caesium-server-auth >/dev/null 2>&1 || true
    cleanup() {
        {{ container_cli }} rm -f caesium-server-auth >/dev/null 2>&1 || true
    }
    trap cleanup EXIT
    {{ container_cli }} run --platform {{ platform }} -d --name caesium-server-auth -p {{ port }}:8080 \
        -v {{ sock }}:/var/run/docker.sock \
        -e DOCKER_HOST=unix:///var/run/docker.sock \
        -e CAESIUM_MANUAL_TRIGGER_API_KEY=e2e-test-key \
        -e CAESIUM_AUTH_MODE=api-key \
        -e CAESIUM_AUTH_KEY_HASH_SECRET=ui-e2e-auth-key-hash-secret-000001 \
        -e CAESIUM_AUTH_REQUIRE_TLS=false \
        -e CAESIUM_OPEN_LINEAGE_ENABLED=true \
        -e CAESIUM_OPEN_LINEAGE_TRANSPORT=console \
        -e CAESIUM_FRESHNESS_ENABLED=true \
        --user 0:0 {{ local_image_ref }}:{{ tag }} start >/dev/null
    tries=0
    until node -e "fetch('http://127.0.0.1:{{ port }}/health').then((r) => process.exit(r.ok ? 0 : 1)).catch(() => process.exit(1))"; do
        tries=$((tries + 1))
        if [ "$tries" -gt 30 ]; then
            echo "Auth-enabled Caesium did not become ready in time" >&2
            {{ container_cli }} logs caesium-server-auth >&2 || true
            exit 1
        fi
        sleep 1
    done
    admin_key=""
    tries=0
    until admin_key="$({{ container_cli }} logs caesium-server-auth 2>&1 | awk '/csk_/ { for (i = 1; i <= NF; i++) if ($i ~ /^csk_/) { print $i; exit } }')" && [ -n "$admin_key" ]; do
        tries=$((tries + 1))
        if [ "$tries" -gt 30 ]; then
            echo "Auth bootstrap admin key did not appear in server logs" >&2
            {{ container_cli }} logs caesium-server-auth >&2 || true
            exit 1
        fi
        sleep 1
    done
    cd ui
    npm ci
    npx playwright install chromium
    CAESIUM_E2E_AUTH_ADMIN_KEY="$admin_key" npm run test:e2e -- --project=auth

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
            --set config.extraEnv[1].name=CAESIUM_FRESHNESS_ENABLED \
            --set-string config.extraEnv[1].value=true \
            --set kubernetes.engine.enabled=true \
            --set persistence.enabled=false \
            --wait
    @echo "Caesium distributed cluster is ready."

# Stop the local Kubernetes deployment
k8s-down:
    helm uninstall caesium

# Run the Phase 0 load harness against a locally-running Caesium server.
# Expects the server to already be up (just run) and reachable at CAESIUM_LOAD_SERVER.
# Writes a JSON/markdown report to docs/load-baseline-YYYY-MM-DD.md by default.
#
# Key env vars (all have defaults):
#   CAESIUM_LOAD_SERVER        — server URL (default: http://127.0.0.1:8080)
#   CAESIUM_LOAD_JOBS          — number of synthetic jobs (default: 10)
#   CAESIUM_LOAD_FAN_OUT       — DAG fan-out width per layer (default: 4)
#   CAESIUM_LOAD_DEPTH         — DAG depth / number of layers (default: 3)
#   CAESIUM_LOAD_TASK_DURATION — per-task sleep duration (default: 1s)
#   CAESIUM_LOAD_CONCURRENCY   — runs triggered in parallel (default: 1)
#   CAESIUM_MANUAL_TRIGGER_API_KEY — API key if auth is enabled
#
load-test:
    #!/usr/bin/env bash
    set -euo pipefail
    report_file="${CAESIUM_LOAD_OUTPUT:-docs/load-baseline-$(date +%Y-%m-%d).md}"
    echo "Running load harness — report will be written to ${report_file}"
    go run ./test/load/harness.go \
        -server "${CAESIUM_LOAD_SERVER:-http://127.0.0.1:8080}" \
        -jobs "${CAESIUM_LOAD_JOBS:-10}" \
        -fan-out "${CAESIUM_LOAD_FAN_OUT:-4}" \
        -depth "${CAESIUM_LOAD_DEPTH:-3}" \
        -task-duration "${CAESIUM_LOAD_TASK_DURATION:-1s}" \
        -concurrency "${CAESIUM_LOAD_CONCURRENCY:-1}" \
        -engine "${CAESIUM_LOAD_ENGINE:-docker}" \
        -api-key "${CAESIUM_MANUAL_TRIGGER_API_KEY:-}" \
        -output "${report_file}"

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
