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
infra_it_container := "caesium-server-infra-test"
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

# The unit-pipeline reagent images (git-source, tf-discover, tf-warm, tf-runner)
# publish under the same Docker Hub org as the product image.
local_reagent_repo := if podman == "true" { "localhost/" + repo } else { repo }
reagent_toolchain_image := repo + "/caesium-reagent-toolchain"
local_reagent_toolchain_ref := if podman == "true" { "localhost/" + reagent_toolchain_image } else { reagent_toolchain_image }
# The single place the Terraform distribution/version is pinned; it flows into
# build/Dockerfile.reagents's ARG defaults. Changing TF_VERSION or TF_DIST also
# requires the matching TF_SHA256_LINUX_* build-args (checksum-verified).
tf_dist := env("CAESIUM_TF_DIST", "terraform")
tf_version := env("CAESIUM_TF_VERSION", "1.15.9")
publish_image_ref := repo + "/" + image
publish_builder_ref := repo + "/" + builder_image
sock := env("CAESIUM_SOCK", default_sock)
port := env("CAESIUM_PORT", "8080")
auth_mode := env("CAESIUM_AUTH_MODE", "none")
event_ingest_api_key := env("CAESIUM_EVENT_INGEST_API_KEY", "integration-test-key")
contract_deprecation_window := env("CAESIUM_CONTRACT_DEPRECATION_WINDOW", "5s")
agent_integration_run := env("CAESIUM_AGENT_INTEGRATION_RUN", "TestIntegrationTestSuite/(TestAgent|TestAuth|TestIncident|TestScoped|TestHold)")
# Minimum number of suite scenarios the auth lane must actually execute. The
# lane used to pass in 0.068s with every scenario skipped on an env guard the
# runner container never received; the recipe now counts `--- PASS` lines and
# fails below this floor so a hollow lane can never be green again.
agent_integration_min_pass := env("CAESIUM_AGENT_INTEGRATION_MIN_PASS", "3")
# Same hollow-lane floor shape as agent_integration_min_pass (H-1), applied to
# every other lane that filters with -run: a `-run` typo or a scenario that
# starts skipping en masse must fail the recipe, not exit 0 having run
# nothing. Chosen as roughly half the observed green-run PASS count.
distributed_integration_min_pass := env("CAESIUM_DISTRIBUTED_INTEGRATION_MIN_PASS", "20")
owner_memory_integration_min_pass := env("CAESIUM_OWNER_MEMORY_INTEGRATION_MIN_PASS", "14")
# Suite-qualified: a bare method name matches no test at all.
infra_integration_run := env("CAESIUM_INFRA_INTEGRATION_RUN", "TestIntegrationTestSuite/TestInfra")
infra_integration_min_pass := env("CAESIUM_INFRA_INTEGRATION_MIN_PASS", "6")
# When true, product-image recipes (build / build-test / build-reagents /
# build-triage-agent) skip if the tagged image already exists. CI sets this
# after docker-loading the images job's artifacts so the integration lanes
# never recompile. Local default is false: a stale image must not silently
# skip a rebuild after you edit source.
skip_image_build := env("CAESIUM_SKIP_IMAGE_BUILD", "false")
# CI loads a small precompiled runner; local runs still compile current sources.
integration_runner_image := env("CAESIUM_INTEGRATION_RUNNER_IMAGE", local_builder_ref + ":" + tag + "-full")
# A deliberately fake deploy key. The infra lane resolves it through the real
# secret://env provider and then asserts the value never reaches a task log —
# it is a canary, not a credential, and it opens nothing.
infra_deploy_key_env := "CAESIUM_INFRA_FAKE_DEPLOY_KEY"
infra_deploy_key_value := "caesium-infra-lane-canary-not-a-real-key"
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
    @if [ "{{ skip_image_build }}" = "true" ]; then \
        echo "Skipping builder (CAESIUM_SKIP_IMAGE_BUILD=true); consumers verify their loaded images."; \
    elif {{ container_cli }} image inspect {{ local_builder_ref }}:{{ tag }} >/dev/null 2>&1; then \
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
    @if [ "{{ skip_image_build }}" = "true" ]; then \
        {{ container_cli }} image inspect {{ local_image_ref }}:{{ tag }} >/dev/null || exit 1; \
        echo "Release image {{ local_image_ref }}:{{ tag }} already present, skipping build (CAESIUM_SKIP_IMAGE_BUILD=true)."; \
    else \
        {{ container_cli }} build --platform {{ platform }} \
            --build-arg BUILDER_IMAGE={{ local_builder_ref }}:{{ tag }} \
            --target release \
            -t {{ local_image_ref }}:{{ tag }} \
            -f {{ dockerfile }} .; \
    fi

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
    @if [ "{{ skip_image_build }}" = "true" ] && {{ container_cli }} image inspect {{ local_image_ref }}:{{ tag }} >/dev/null 2>&1; then \
        echo "Release image {{ local_image_ref }}:{{ tag }} already present, skipping build (CAESIUM_SKIP_IMAGE_BUILD=true)."; \
    else \
        {{ container_cli }} build --platform {{ platform }} \
            --build-arg BUILDER_IMAGE={{ local_builder_ref }}:{{ tag }} \
            --target release \
            -t {{ local_image_ref }}:{{ tag }} \
            -f {{ dockerfile }} .; \
    fi

build-test: builder
    @if [ "{{ skip_image_build }}" = "true" ]; then \
        {{ container_cli }} image inspect {{ local_image_ref }}:{{ tag }}-test >/dev/null || exit 1; \
        echo "Test image {{ local_image_ref }}:{{ tag }}-test already present, skipping build (CAESIUM_SKIP_IMAGE_BUILD=true)."; \
    else \
        {{ container_cli }} build --platform {{ platform }} \
            --build-arg BUILDER_IMAGE={{ local_builder_ref }}:{{ tag }} \
            --target test \
            -t {{ local_image_ref }}:{{ tag }}-test \
            -f {{ dockerfile }} .; \
    fi

build-triage-agent: validate-platform
    @if [ "{{ skip_image_build }}" = "true" ] && {{ container_cli }} image inspect {{ local_triage_agent_ref }}:{{ tag }} >/dev/null 2>&1; then \
        echo "Triage agent image {{ local_triage_agent_ref }}:{{ tag }} already present, skipping build (CAESIUM_SKIP_IMAGE_BUILD=true)."; \
    else \
        {{ container_cli }} build --platform {{ platform }} \
            -t {{ local_triage_agent_ref }}:{{ tag }} \
            -t {{ triage_agent_image }}:latest \
            -f build/Dockerfile.triage-agent .; \
    fi

# ---------------------------------------------------------------------------
# The unit-pipeline reagents (docs/superpowers/specs/…-infrastructure-deployment…).
# `reagents/` is a separate Go module, so it is invisible to the root `./...`
# and needs its own lint/test targets — these are wired into CI's lint and
# unit-test jobs alongside the root ones.
# ---------------------------------------------------------------------------

# Build the reagent toolchain image (Go + golangci-lint + pinned terraform).
reagents-toolchain: validate-platform
    @if [ "{{ skip_image_build }}" = "true" ] && {{ container_cli }} image inspect {{ local_reagent_toolchain_ref }}:{{ tag }} >/dev/null 2>&1; then \
        echo "Reagent toolchain image {{ local_reagent_toolchain_ref }}:{{ tag }} already exists, skipping build."; \
    else \
        {{ container_cli }} build --platform {{ platform }} \
            --build-arg TF_DIST={{ tf_dist }} \
            --build-arg TF_VERSION={{ tf_version }} \
            --target toolchain \
            -t {{ local_reagent_toolchain_ref }}:{{ tag }} \
            -f build/Dockerfile.reagents .; \
    fi

# gofmt + go vet + golangci-lint over the nested reagents module.
reagents-lint: reagents-toolchain
    {{ container_cli }} run --rm --platform {{ platform }} \
        -v {{ repo_dir }}:{{ bld_dir }} \
        -w {{ bld_dir }}/reagents \
        -e GOFLAGS=-buildvcs=false \
        {{ local_reagent_toolchain_ref }}:{{ tag }} \
        sh -c 'set -eu; \
            unformatted=$(gofmt -l .); \
            if [ -n "$unformatted" ]; then echo "gofmt needed:"; echo "$unformatted"; exit 1; fi; \
            go vet ./...; \
            golangci-lint run ./...'

# Runs inside the toolchain stage so `terraform` is on PATH for the tests that
# drive it. The suite is hermetic: `terraform get` installs modules only (never
# providers) and the fixture's module sources are all relative, while
# git-source clones over file://. Verified to pass under `--network none`.

# Run the reagents module's unit tests (terraform on PATH; no network needed).
reagents-test: reagents-toolchain
    {{ container_cli }} run --rm --platform {{ platform }} \
        -v {{ repo_dir }}:{{ bld_dir }} \
        -w {{ bld_dir }}/reagents \
        -e GOFLAGS=-buildvcs=false \
        {{ local_reagent_toolchain_ref }}:{{ tag }} \
        go test -race ./...

# Build all four reagent role images (--target per role in Dockerfile.reagents).
build-reagents: validate-platform
    #!/usr/bin/env bash
    set -euo pipefail
    missing=0
    for role in git-source tf-discover tf-warm tf-runner; do
        if ! {{ container_cli }} image inspect "{{ local_reagent_repo }}/$role:{{ tag }}" >/dev/null 2>&1; then
            missing=1
            break
        fi
    done
    if [ "{{ skip_image_build }}" = "true" ] && [ "$missing" -eq 0 ]; then
        echo "Reagent role images already present, skipping build (CAESIUM_SKIP_IMAGE_BUILD=true)."
        exit 0
    fi
    for role in git-source tf-discover tf-warm tf-runner; do
        {{ container_cli }} build --platform {{ platform }} \
            --build-arg TF_DIST={{ tf_dist }} \
            --build-arg TF_VERSION={{ tf_version }} \
            --target "$role" \
            -t "{{ local_reagent_repo }}/$role:{{ tag }}" \
            -t "{{ repo }}/$role:latest" \
            -f build/Dockerfile.reagents .
    done

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

# Put a runnable `caesium` CLI on the host at ./.tmp/caesium-cli/caesium.
#
#   just tag=v0.1.0 cli     # a specific release
#   just cli                # newest published release (resolved with gh)
#
# On Linux the statically linked binary attached to the release is used when
# it is available (a real host binary, no container). Everywhere else — macOS
# included — the CLI is a thin wrapper that runs the release image, because
# the executable inside that image is dynamically linked against musl,
# libdqlite, libuv, lz4 and sqlite3 and is not portable to the host.
cli:
    #!/usr/bin/env bash
    set -euo pipefail

    tag="{{ tag }}"
    if [ "$tag" = "latest" ]; then
        if ! command -v gh >/dev/null 2>&1; then
            echo "error: resolving the newest release needs the GitHub CLI (gh)." >&2
            echo "       Install gh, or pass a tag explicitly: just tag=vX.Y.Z cli" >&2
            exit 1
        fi
        tag="$(gh release view --json tagName --jq .tagName 2>/dev/null || true)"
        if [ -z "$tag" ]; then
            echo "error: this repository has no published GitHub release yet." >&2
            echo "       Pass a tag explicitly once one exists: just tag=vX.Y.Z cli" >&2
            exit 1
        fi
        echo "Resolved latest release: ${tag}"
    fi

    out_dir="{{ repo_dir }}/.tmp/caesium-cli"
    mkdir -p "$out_dir"

    # Preferred on Linux: the static binary attached to the release.
    if [ "$(uname -s)" = "Linux" ] && command -v gh >/dev/null 2>&1; then
        case "$(uname -m)" in
            x86_64|amd64) asset="caesium-linux-amd64" ;;
            aarch64|arm64) asset="caesium-linux-arm64" ;;
            *) asset="" ;;
        esac
        if [ -n "$asset" ] && \
           gh release download "$tag" --pattern "$asset" \
               --output "$out_dir/caesium" --clobber >/dev/null 2>&1; then
            chmod +x "$out_dir/caesium"
            echo "Installed the static ${asset} from release ${tag}:"
            echo "  $out_dir/caesium"
            exit 0
        fi
        echo "No ${asset:-static} asset on release ${tag}; falling back to the container wrapper."
    fi

    image="{{ publish_image_ref }}:${tag}"
    if {{ container_cli }} image inspect "$image" >/dev/null 2>&1; then
        echo "Using local image ${image} (skipping pull)."
    elif ! {{ container_cli }} pull "$image"; then
        echo "error: image ${image} does not exist (or is not pullable)." >&2
        echo "       Check the tag against https://hub.docker.com/r/{{ publish_image_ref }}/tags" >&2
        exit 1
    fi

    # The image ENTRYPOINT is /bin/caesium, so --entrypoint is what makes
    # `caesium <args>` work; every CAESIUM_* variable set on the host is
    # forwarded into the container.
    cat > "$out_dir/caesium" <<WRAPPER
    #!/usr/bin/env sh
    # Generated by \`just tag=${tag} cli\`. Runs the Caesium CLI from
    # {{ publish_image_ref }}:${tag} against the current directory.
    set -e
    env_flags=""
    for name in \$(env | sed -n 's/^\(CAESIUM_[A-Za-z0-9_]*\)=.*/\1/p'); do
        env_flags="\$env_flags -e \$name"
    done
    # Linux: host networking, so --server http://localhost:8080 reaches a server
    # on this host. macOS/other: Docker Desktop's host network is the VM's, not
    # the Mac's, so map host.docker.internal to the host gateway instead and
    # address the server as http://host.docker.internal:8080.
    if [ "\$(uname -s)" = "Linux" ]; then
        net_flags="--network host"
    else
        net_flags="--add-host host.docker.internal:host-gateway"
    fi
    exec {{ container_cli }} run --rm \$net_flags \\
        \$env_flags \\
        -v "\$PWD":/work -w /work \\
        --user "\$(id -u):\$(id -g)" \\
        --entrypoint /bin/caesium \\
        {{ publish_image_ref }}:${tag} "\$@"
    WRAPPER
    chmod +x "$out_dir/caesium"
    echo "Wrote a ${image} CLI wrapper to:"
    echo "  $out_dir/caesium"
    if [ "$(uname -s)" != "Linux" ]; then
        echo "note: on $(uname -s) the wrapper cannot use host networking; address a server on this machine as http://host.docker.internal:8080"
    fi
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

# A selected precompiled runner must exist. Otherwise prepare the normal local
# compiler image once, before starting any integration server.
integration-runner: validate-platform
    @if [ -n "${CAESIUM_INTEGRATION_RUNNER_IMAGE:-}" ]; then \
        {{ container_cli }} image inspect {{ integration_runner_image }} >/dev/null; \
    else \
        just tag={{ tag }} builder-full; \
    fi

integration-test: integration-runner
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
        -e CAESIUM_TEST_SHARD_INDEX \
        -e CAESIUM_TEST_SHARD_COUNT \
        -e CAESIUM_EVENT_INGEST_API_KEY={{ event_ingest_api_key }} \
        -e DOCKER_HOST=unix:///var/run/docker.sock \
        --network=container:{{ it_container }} \
        -w {{ bld_dir }} \
        {{ integration_runner_image }} \
        sh -c 'sh scripts/integration-test.sh'; then \
      {{ container_cli }} rm -f {{ it_container }} >/dev/null 2>&1 || true; \
    else \
      echo "integration tests failed; caesium server logs:"; \
      {{ container_cli }} logs {{ it_container }} || true; \
      {{ container_cli }} rm -f {{ it_container }} >/dev/null 2>&1 || true; \
      exit 1; \
    fi

# The -test.run list below is the set of scenarios whose BEHAVIOUR differs in
# distributed mode, so each one must name a path the local lane cannot reach.
# TestDataAssertionsMetricsPersisted is there for the dataset-metric CLAIM FENCE
# (issue #438): the worker's post-task seam writes samples only while it still
# holds the TaskRun row's claim, and that fence exists ONLY on this lane — a
# wrong claim identity would silently drop every distributed sample, which the
# local-mode default lane cannot observe.
integration-test-distributed: integration-runner
    just tag={{ tag }} integration-up-distributed
    @cli_dir={{ repo_dir }}/.tmp/caesium-cli; \
    rm -rf "$cli_dir"; \
    mkdir -p "$cli_dir"; \
    cli_ctr=$({{ container_cli }} create --platform {{ platform }} {{ local_image_ref }}:{{ tag }}-test true); \
    trap '{{ container_cli }} rm -f "$cli_ctr" >/dev/null 2>&1 || true; rm -rf "$cli_dir"' EXIT; \
    {{ container_cli }} cp "$cli_ctr":/bin/caesium "$cli_dir/caesium"; \
    chmod +x "$cli_dir/caesium"; \
    {{ container_cli }} rm -f "$cli_ctr" >/dev/null 2>&1 || true; \
    log={{ repo_dir }}/.tmp/integration-test-distributed.log; \
    rm -f "$log" "$log.rc"; \
    { {{ container_cli }} run --rm --platform {{ platform }} \
        -v {{ repo_dir }}:{{ bld_dir }} \
        -v {{ sock }}:/var/run/docker.sock \
        -e CAESIUM_CLI_PATH={{ bld_dir }}/.tmp/caesium-cli/caesium \
        -e CAESIUM_EVENT_INGEST_API_KEY={{ event_ingest_api_key }} \
        -e CAESIUM_EXECUTION_MODE=distributed \
        -e DOCKER_HOST=unix:///var/run/docker.sock \
        --network=container:{{ it_container }} \
        -w {{ bld_dir }} \
        {{ integration_runner_image }} \
        sh -c 'sh scripts/integration-test.sh -test.run "TestIntegrationTestSuite/(TestRunConcurrencyStrategies|TestPriorityRunStartSurfacesAndCronDefault|TestFanOut|TestPlainFailure|TestReplaceCancel|TestRetryAfterApplyExecutesRegisteredCommand|TestRetryValidatesAgainstTheRegisteredOutputSchema|TestDataAssertionsMetricsPersisted)"' 2>&1; echo $? >"$log.rc"; } | tee "$log"; \
    rc=$(cat "$log.rc"); \
    passes=$(grep -cE '^[[:space:]]*--- PASS: TestIntegrationTestSuite/' "$log" 2>/dev/null || true); \
    passes=${passes:-0}; \
    if [ "$rc" -ne 0 ]; then \
      echo "distributed integration tests failed; caesium server logs:"; \
      {{ container_cli }} logs {{ it_container }} || true; \
      {{ container_cli }} rm -f {{ it_container }} >/dev/null 2>&1 || true; \
      exit 1; \
    fi; \
    if [ "$passes" -lt {{ distributed_integration_min_pass }} ]; then \
      echo "distributed integration lane executed only $passes scenario(s), expected at least {{ distributed_integration_min_pass }}: the lane is hollow (guards skipped almost everything)"; \
      {{ container_cli }} logs {{ it_container }} || true; \
      {{ container_cli }} rm -f {{ it_container }} >/dev/null 2>&1 || true; \
      exit 1; \
    fi; \
    echo "distributed integration lane executed $passes scenario(s) (minimum {{ distributed_integration_min_pass }})"; \
    {{ container_cli }} rm -f {{ it_container }} >/dev/null 2>&1 || true

integration-test-owner-memory: integration-runner
    just tag={{ tag }} integration-up-owner-memory
    @cli_dir={{ repo_dir }}/.tmp/caesium-cli; \
    rm -rf "$cli_dir"; \
    mkdir -p "$cli_dir"; \
    cli_ctr=$({{ container_cli }} create --platform {{ platform }} {{ local_image_ref }}:{{ tag }}-test true); \
    trap '{{ container_cli }} rm -f "$cli_ctr" >/dev/null 2>&1 || true; rm -rf "$cli_dir"' EXIT; \
    {{ container_cli }} cp "$cli_ctr":/bin/caesium "$cli_dir/caesium"; \
    chmod +x "$cli_dir/caesium"; \
    {{ container_cli }} rm -f "$cli_ctr" >/dev/null 2>&1 || true; \
    log={{ repo_dir }}/.tmp/integration-test-owner-memory.log; \
    rm -f "$log" "$log.rc"; \
    { {{ container_cli }} run --rm --platform {{ platform }} \
        -v {{ repo_dir }}:{{ bld_dir }} \
        -v {{ sock }}:/var/run/docker.sock \
        -e CAESIUM_CLI_PATH={{ bld_dir }}/.tmp/caesium-cli/caesium \
        -e CAESIUM_EVENT_INGEST_API_KEY={{ event_ingest_api_key }} \
        -e CAESIUM_EXECUTION_MODE=distributed \
        -e CAESIUM_RUN_OWNER_IN_MEMORY=true \
        -e DOCKER_HOST=unix:///var/run/docker.sock \
        --network=container:{{ it_container }} \
        -w {{ bld_dir }} \
        {{ integration_runner_image }} \
        sh -c 'sh scripts/integration-test.sh -test.run "TestIntegrationTestSuite/(TestFanOut|TestPlainFailure)"' 2>&1; echo $? >"$log.rc"; } | tee "$log"; \
    rc=$(cat "$log.rc"); \
    passes=$(grep -cE '^[[:space:]]*--- PASS: TestIntegrationTestSuite/' "$log" 2>/dev/null || true); \
    passes=${passes:-0}; \
    if [ "$rc" -ne 0 ]; then \
      echo "owner-memory integration tests failed; caesium server logs:"; \
      {{ container_cli }} logs {{ it_container }} || true; \
      {{ container_cli }} rm -f {{ it_container }} >/dev/null 2>&1 || true; \
      exit 1; \
    fi; \
    if [ "$passes" -lt {{ owner_memory_integration_min_pass }} ]; then \
      echo "owner-memory integration lane executed only $passes scenario(s), expected at least {{ owner_memory_integration_min_pass }}: the lane is hollow (guards skipped almost everything)"; \
      {{ container_cli }} logs {{ it_container }} || true; \
      {{ container_cli }} rm -f {{ it_container }} >/dev/null 2>&1 || true; \
      exit 1; \
    fi; \
    echo "owner-memory integration lane executed $passes scenario(s) (minimum {{ owner_memory_integration_min_pass }})"; \
    {{ container_cli }} rm -f {{ it_container }} >/dev/null 2>&1 || true

# The runner container is given the same CAESIUM_AUTH_MODE /
# CAESIUM_AGENT_REMEDIATION_ENABLED / CAESIUM_AUTH_KEY_HASH_SECRET that
# `integration-up-agent` gives the server. Keep the hash secret in sync with it:
# a mismatch makes every agent-session key the suite mints unlookupable, so the
# server answers 401 where the scenario expects 200.

# Run the integration suite against an auth-enabled, remediation-enabled server.
integration-test-agent: integration-runner
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
    log={{ repo_dir }}/.tmp/integration-test-agent.log; \
    rm -f "$log"; \
    rc=0; \
    {{ container_cli }} run --rm --platform {{ platform }} \
        -v {{ repo_dir }}:{{ bld_dir }} \
        -v {{ sock }}:/var/run/docker.sock \
        -e CAESIUM_CLI_PATH={{ bld_dir }}/.tmp/caesium-cli/caesium \
        -e CAESIUM_MANUAL_TRIGGER_API_KEY=integration-test-key \
        -e CAESIUM_EVENT_INGEST_API_KEY={{ event_ingest_api_key }} \
        -e CAESIUM_AUTH_ADMIN_KEY="$admin_key" \
        -e CAESIUM_API_KEY="$admin_key" \
        -e CAESIUM_AGENT_AUTH_LANE=true \
        -e CAESIUM_AUTH_MODE=api-key \
        -e CAESIUM_AGENT_REMEDIATION_ENABLED=true \
        -e CAESIUM_AUTH_KEY_HASH_SECRET=agent-integration-auth-key-hash-secret-000001 \
        -e CAESIUM_TRIAGE_AGENT_IMAGE={{ triage_agent_image }}:latest \
        -e DOCKER_HOST=unix:///var/run/docker.sock \
        --network=container:{{ agent_it_container }} \
        -w {{ bld_dir }} \
        {{ integration_runner_image }} \
        sh -c 'sh scripts/integration-test.sh -test.run "{{ agent_integration_run }}"' >"$log" 2>&1 || rc=$?; \
    cat "$log"; \
    passes=$(grep -cE '^[[:space:]]*--- PASS: TestIntegrationTestSuite/' "$log" 2>/dev/null || true); \
    passes=${passes:-0}; \
    if [ "$rc" -ne 0 ]; then \
      echo "agent auth integration tests failed; caesium server logs:"; \
      {{ container_cli }} logs {{ agent_it_container }} || true; \
      {{ container_cli }} rm -f {{ agent_it_container }} >/dev/null 2>&1 || true; \
      exit 1; \
    fi; \
    if [ "$passes" -lt {{ agent_integration_min_pass }} ]; then \
      echo "agent auth lane executed only $passes scenario(s), expected at least {{ agent_integration_min_pass }}: the lane is hollow (guards skipped every scenario)"; \
      {{ container_cli }} logs {{ agent_it_container }} || true; \
      {{ container_cli }} rm -f {{ agent_it_container }} >/dev/null 2>&1 || true; \
      exit 1; \
    fi; \
    echo "agent auth lane executed $passes scenario(s) (minimum {{ agent_integration_min_pass }})"; \
    {{ container_cli }} rm -f {{ agent_it_container }} >/dev/null 2>&1 || true

integration-down:
    {{ container_cli }} rm -f {{ it_container }}

integration-down-agent:
    {{ container_cli }} rm -f {{ agent_it_container }}

integration-down-infra:
    {{ container_cli }} rm -f {{ infra_it_container }}

# Run integration tests against a Caesium server using the Podman engine.
# Requires Podman to be installed and the Podman socket to be active

# (run: systemctl --user enable --now podman.socket).
integration-test-podman: build integration-runner
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
        -e CAESIUM_DATA_ASSERTIONS_ENABLED=true \
        -e CAESIUM_CONTRACT_ENFORCEMENT=fail \
        -e CAESIUM_CONTRACT_DEPRECATION_WINDOW={{ contract_deprecation_window }} \
        -e CAESIUM_CACHE_PIN_DIGESTS=true \
        -e CAESIUM_CACHE_ENABLED=true \
        -e CAESIUM_CANCEL_RECONCILE_INTERVAL=2s \
        --user 0:0 \
        {{ repo }}/{{ image }}:{{ tag }} start
    cli_dir={{ repo_dir }}/.tmp/caesium-cli-podman
    mkdir -p "$cli_dir"
    trap 'rm -rf "$cli_dir"; docker rm -f caesium-server-podman >/dev/null 2>&1 || true' EXIT
    docker cp caesium-server-podman:/bin/caesium "$cli_dir/caesium"
    chmod +x "$cli_dir/caesium"
    if docker run --rm --platform {{ platform }} \
        -v {{ repo_dir }}:{{ bld_dir }} \
        -e CAESIUM_TEST_ENGINE=podman \
        -e CAESIUM_CLI_PATH={{ bld_dir }}/.tmp/caesium-cli-podman/caesium \
        -e CAESIUM_TEST_SHARD_INDEX \
        -e CAESIUM_TEST_SHARD_COUNT \
        -e CAESIUM_EVENT_INGEST_API_KEY={{ event_ingest_api_key }} \
        --network=container:caesium-server-podman \
        -w {{ bld_dir }} \
        {{ integration_runner_image }} \
        sh -c 'sh scripts/integration-test.sh'; then
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
        -e CAESIUM_DATA_ASSERTIONS_ENABLED=true \
        -e CAESIUM_CONTRACT_ENFORCEMENT=fail \
        -e CAESIUM_CONTRACT_DEPRECATION_WINDOW={{ contract_deprecation_window }} \
        -e CAESIUM_CACHE_PIN_DIGESTS=true \
        -e CAESIUM_CACHE_ENABLED=true \
        -e CAESIUM_NOTIFICATION_WATCHER_INTERVAL=1s \
        -e CAESIUM_RATE_LIMIT_PRUNER_ENABLED=true \
        -e CAESIUM_RATE_LIMIT_PRUNE_INTERVAL=500ms \
        -e CAESIUM_RUN_QUEUE_ENABLED=true \
        -e CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED=true \
        -e CAESIUM_RUN_QUEUE_DEQUEUE_INTERVAL=500ms \
        -e CAESIUM_FANOUT_MAX_PARTITIONS=8 \
        -e CAESIUM_CANCEL_RECONCILE_INTERVAL=2s \
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
        -e CAESIUM_DATA_ASSERTIONS_ENABLED=true \
        -e CAESIUM_CONTRACT_ENFORCEMENT=fail \
        -e CAESIUM_CONTRACT_DEPRECATION_WINDOW={{ contract_deprecation_window }} \
        -e CAESIUM_CACHE_PIN_DIGESTS=true \
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
        -e CAESIUM_FANOUT_MAX_PARTITIONS=8 \
        {{ local_image_ref }}:{{ tag }}-test start

integration-up-owner-memory: build-test
    {{ container_cli }} rm -f {{ it_container }} >/dev/null 2>&1 || true
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
        -e CAESIUM_DATA_ASSERTIONS_ENABLED=true \
        -e CAESIUM_CONTRACT_ENFORCEMENT=fail \
        -e CAESIUM_CONTRACT_DEPRECATION_WINDOW={{ contract_deprecation_window }} \
        -e CAESIUM_CACHE_PIN_DIGESTS=true \
        -e CAESIUM_NOTIFICATION_WATCHER_INTERVAL=1s \
        -e CAESIUM_EXECUTION_MODE=distributed \
        -e CAESIUM_NODE_ADDRESS=127.0.0.1:9001 \
        -e CAESIUM_INTERNAL_WAKEUP_TOKEN=integration-distributed-internal-token \
        -e CAESIUM_INTERNAL_PORT=8443 \
        -e CAESIUM_RUN_OWNER_ENABLED=true \
        -e CAESIUM_RUN_OWNER_IN_MEMORY=true \
        -e CAESIUM_RUN_LEASE_TTL=30s \
        -e CAESIUM_RUN_OWNER_DISPATCH_INTERVAL=500ms \
        -e CAESIUM_RUN_OWNER_DISPATCH_DEADLINE=5m \
        -e CAESIUM_RUN_OWNER_DISPATCH_PROGRESS_DEADLINE=5s \
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
        -e CAESIUM_FANOUT_MAX_PARTITIONS=8 \
        {{ local_image_ref }}:{{ tag }}-test start

# The infra lane runs its own server so it can coexist with the default lane,
# and it builds the reagent images the TestInfra scenarios mount. Those
# scenarios skip everywhere else (see CAESIUM_INFRA_LANE in
# test/infra_fixture_test.go): the podman, helm and kubernetes lanes bring up
# their own servers without the reagent images and would drift red otherwise.
#
# CAESIUM_CACHE_ENABLED is set here (and, since #368, on integration-up too)
# because the whole point of this feature is change-gating: without it every
# "must not be cached" assertion passes vacuously and the cache-behaviour
# scenarios would measure nothing. Every cache scenario in test/ pins its own
# behavior via `metadata.cache: true` / `cache: false` (pkg/jobdef.applyCache
# layers a job/step declaration over the env default, so an explicit
# declaration always wins), so turning this on here does not change what any
# existing scenario asserts — it just makes the server-wide default itself
# exercised too, on every lane that starts its own server (this one, the
# default lane, podman, and the ui-e2e/ui-e2e-auth blocks in ci.yml).

# Start the infra lane's server (own container, reagent images built).
integration-up-infra: build-test build-reagents
    {{ container_cli }} rm -f {{ infra_it_container }} >/dev/null 2>&1 || true
    {{ container_cli }} run -d --platform {{ platform }} \
        --name {{ infra_it_container }} \
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
        -e CAESIUM_DATA_ASSERTIONS_ENABLED=true \
        -e CAESIUM_CONTRACT_ENFORCEMENT=fail \
        -e CAESIUM_CONTRACT_DEPRECATION_WINDOW={{ contract_deprecation_window }} \
        -e CAESIUM_CACHE_PIN_DIGESTS=true \
        -e CAESIUM_NOTIFICATION_WATCHER_INTERVAL=1s \
        -e CAESIUM_RATE_LIMIT_PRUNER_ENABLED=true \
        -e CAESIUM_RATE_LIMIT_PRUNE_INTERVAL=500ms \
        -e CAESIUM_RUN_QUEUE_ENABLED=true \
        -e CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED=true \
        -e CAESIUM_RUN_QUEUE_DEQUEUE_INTERVAL=500ms \
        -e CAESIUM_FANOUT_MAX_PARTITIONS=8 \
        -e CAESIUM_CACHE_ENABLED=true \
        -e {{ infra_deploy_key_env }}={{ infra_deploy_key_value }} \
        {{ local_image_ref }}:{{ tag }}-test start

# Run the TestInfra scenarios against the infra lane's server.
integration-test-infra: integration-runner
    just tag={{ tag }} integration-up-infra
    @cli_dir={{ repo_dir }}/.tmp/caesium-cli-infra; \
    rm -rf "$cli_dir"; \
    mkdir -p "$cli_dir"; \
    cli_ctr=$({{ container_cli }} create --platform {{ platform }} {{ local_image_ref }}:{{ tag }}-test true); \
    trap '{{ container_cli }} rm -f "$cli_ctr" >/dev/null 2>&1 || true; rm -rf "$cli_dir"' EXIT; \
    {{ container_cli }} cp "$cli_ctr":/bin/caesium "$cli_dir/caesium"; \
    chmod +x "$cli_dir/caesium"; \
    {{ container_cli }} rm -f "$cli_ctr" >/dev/null 2>&1 || true; \
    log={{ repo_dir }}/.tmp/integration-test-infra.log; \
    rm -f "$log" "$log.rc"; \
    { {{ container_cli }} run --rm --platform {{ platform }} \
        -v {{ repo_dir }}:{{ bld_dir }} \
        -v {{ sock }}:/var/run/docker.sock \
        -e CAESIUM_CLI_PATH={{ bld_dir }}/.tmp/caesium-cli-infra/caesium \
        -e CAESIUM_EVENT_INGEST_API_KEY={{ event_ingest_api_key }} \
        -e DOCKER_HOST=unix:///var/run/docker.sock \
        -e CAESIUM_INFRA_LANE=true \
        -e CAESIUM_REAGENT_IMAGE_TAG={{ tag }} \
        -e CAESIUM_HOST_PROJECT_ROOT={{ repo_dir }} \
        -e CAESIUM_INFRA_DEPLOY_KEY_REF=secret://env/{{ infra_deploy_key_env }} \
        -e CAESIUM_INFRA_DEPLOY_KEY_CANARY={{ infra_deploy_key_value }} \
        --network=container:{{ infra_it_container }} \
        -w {{ bld_dir }} \
        {{ integration_runner_image }} \
        sh -c 'sh scripts/integration-test.sh -test.run "{{ infra_integration_run }}" -test.timeout=20m' 2>&1; echo $? >"$log.rc"; } | tee "$log"; \
    rc=$(cat "$log.rc"); \
    passes=$(grep -cE '^[[:space:]]*--- PASS: TestIntegrationTestSuite/' "$log" 2>/dev/null || true); \
    passes=${passes:-0}; \
    if [ "$rc" -ne 0 ]; then \
      echo "infra integration tests failed; caesium server logs:"; \
      {{ container_cli }} logs {{ infra_it_container }} || true; \
      {{ container_cli }} rm -f {{ infra_it_container }} >/dev/null 2>&1 || true; \
      exit 1; \
    fi; \
    if [ "$passes" -lt {{ infra_integration_min_pass }} ]; then \
      echo "infra integration lane executed only $passes scenario(s), expected at least {{ infra_integration_min_pass }}: the lane is hollow (guards skipped almost everything)"; \
      {{ container_cli }} logs {{ infra_it_container }} || true; \
      {{ container_cli }} rm -f {{ infra_it_container }} >/dev/null 2>&1 || true; \
      exit 1; \
    fi; \
    echo "infra integration lane executed $passes scenario(s) (minimum {{ infra_integration_min_pass }})"; \
    {{ container_cli }} rm -f {{ infra_it_container }} >/dev/null 2>&1 || true

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
        -e CAESIUM_AGENT_APPROVAL_REDRIVE_INTERVAL=5s \
        -e CAESIUM_API_EXTERNAL_URL={{ agent_api_external_url }} \
        -e CAESIUM_OPEN_LINEAGE_ENABLED=true \
        -e CAESIUM_OPEN_LINEAGE_TRANSPORT=console \
        -e CAESIUM_FRESHNESS_ENABLED=true \
        -e CAESIUM_DATA_ASSERTIONS_ENABLED=true \
        -e CAESIUM_CONTRACT_ENFORCEMENT=fail \
        -e CAESIUM_CONTRACT_DEPRECATION_WINDOW={{ contract_deprecation_window }} \
        -e CAESIUM_CACHE_PIN_DIGESTS=true \
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

# Single npm ci covering lint + unit tests + production build. CI uses this
# so the two recipes above do not each reinstall node_modules.
ui-ci: builder-full
    {{ container_cli }} run --rm --platform {{ platform }} \
        -v {{ repo_dir }}:{{ bld_dir }} \
        -w {{ bld_dir }}/ui \
        {{ local_builder_ref }}:{{ tag }}-full \
        sh -c 'npm ci --prefer-offline && npm run lint && npm test && npm run build:ci'

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
            -e CAESIUM_DATA_ASSERTIONS_ENABLED=true \
            -e CAESIUM_CONTRACT_ENFORCEMENT=fail \
            -e CAESIUM_CONTRACT_DEPRECATION_WINDOW={{ contract_deprecation_window }} \
            -e CAESIUM_CACHE_PIN_DIGESTS=true \
            -e CAESIUM_CACHE_ENABLED=true \
            -e CAESIUM_RUN_QUEUE_ENABLED=true \
            -e CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED=true \
            -e CAESIUM_RUN_QUEUE_DEQUEUE_INTERVAL=500ms \
            --user 0:0 {{ local_image_ref }}:{{ tag }} start >/dev/null; \
        tries=0; \
        until curl -sf http://127.0.0.1:{{ port }}/health >/dev/null; do \
            tries=$((tries + 1)); \
            if [ "$tries" -gt 30 ]; then \
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
        -e CAESIUM_DATA_ASSERTIONS_ENABLED=true \
        -e CAESIUM_CONTRACT_ENFORCEMENT=fail \
        -e CAESIUM_CONTRACT_DEPRECATION_WINDOW={{ contract_deprecation_window }} \
        -e CAESIUM_CACHE_PIN_DIGESTS=true \
        -e CAESIUM_CACHE_ENABLED=true \
        -e CAESIUM_AGENT_REMEDIATION_ENABLED=true \
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
            --set config.extraEnv[2].name=CAESIUM_CONTRACT_ENFORCEMENT \
            --set-string config.extraEnv[2].value=fail \
            --set config.extraEnv[3].name=CAESIUM_CACHE_PIN_DIGESTS \
            --set-string config.extraEnv[3].value=true \
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

# Whether `clean-worktrees` actually deletes. Default is a dry run:
#   just clean-worktrees              # list what would go
#   just force=true clean-worktrees   # delete it
force := "false"

# Prune the agent worktrees under .claude/worktrees/.
#
# Parallel exec-plan waves leave one `git worktree` per sub-agent behind. By
# 2026-09-07 there were 18 checkouts under .claude/worktrees/ — roughly 675k
# lines of duplicated tree — long after every one of their branches had been
# squash-merged into master. Nothing removed them, because `git worktree prune`
# only drops entries whose directory is already gone.
#
# A checkout is removable when it has no uncommitted changes AND its work has
# landed on master: reachable from master (a real merge or fast-forward), a tree
# identical to master, or a merged pull request whose merged head is EXACTLY this
# checkout's HEAD. That last check is what covers this repo's squash-merge
# workflow, where a branch's commits never become master's ancestors; comparing
# against the PR's merged head (not just "a merged PR exists for this branch") is
# what stops a branch with newer, unmerged commits from being deleted.
#
# A checkout sitting exactly ON the master tip is never removable, whatever the
# other tests say: that is a freshly-branched agent that has not committed yet,
# and both "reachable from master" and "tree identical to master" match it.
#
# Everything else is reported and left alone — a live agent's lane is never
# someone else's to delete. The recipe runs from any checkout: it always operates
# on the MAIN worktree's .claude/worktrees/.
clean-worktrees:
    #!/usr/bin/env bash
    set -uo pipefail
    self="$(git -C "{{ repo_dir }}" rev-parse --show-toplevel)"
    main_wt="$(git -C "{{ repo_dir }}" worktree list --porcelain | awk '/^worktree /{print substr($0, 10); exit}')"
    cd "$main_wt" || exit 1
    git worktree prune
    have_gh=no
    command -v gh >/dev/null 2>&1 && have_gh=yes
    master_tip="$(git rev-parse master)"
    removable=0
    kept=0
    for wt in .claude/worktrees/*/; do
        [ -d "$wt" ] || continue
        dir="$(cd "$wt" && pwd)"
        if [ "$dir" = "$self" ]; then
            echo "KEEP    $wt (this checkout)"
            kept=$((kept + 1))
            continue
        fi
        branch="$(git -C "$dir" symbolic-ref --quiet --short HEAD || echo '(detached)')"
        if [ -n "$(git -C "$dir" status --porcelain)" ]; then
            echo "KEEP    $wt [$branch] uncommitted changes"
            kept=$((kept + 1))
            continue
        fi
        head="$(git -C "$dir" rev-parse HEAD)"
        if [ "$head" = "$master_tip" ]; then
            echo "KEEP    $wt [$branch] sitting on the master tip (nothing committed yet)"
            kept=$((kept + 1))
            continue
        fi
        landed=""
        if git merge-base --is-ancestor "$head" master 2>/dev/null; then
            landed="reachable from master"
        elif git diff --quiet master "$head" 2>/dev/null; then
            landed="tree identical to master"
        elif [ "$have_gh" = yes ] && [ "$branch" != "(detached)" ]; then
            merged_head="$(gh pr list --state merged --head "$branch" --limit 1 --json headRefOid --jq '.[].headRefOid' 2>/dev/null)"
            if [ -n "$merged_head" ] && [ "$merged_head" = "$head" ]; then
                landed="merged pull request at this exact HEAD"
            fi
        fi
        if [ -n "$landed" ]; then
            echo "REMOVE  $wt [$branch] ($landed)"
            removable=$((removable + 1))
            if [ "{{ force }}" = "true" ]; then
                git worktree remove --force "$dir"
            fi
        else
            echo "KEEP    $wt [$branch] not landed on master"
            kept=$((kept + 1))
        fi
    done
    echo
    if [ "{{ force }}" != "true" ]; then
        echo "dry run: $removable removable, $kept kept. Re-run with 'just force=true clean-worktrees' to delete."
    else
        echo "removed $removable worktree(s), kept $kept."
    fi
