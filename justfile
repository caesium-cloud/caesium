repo 			:= "caesiumcloud"
image			:= "caesium"
builder_image	:= image + "-builder"
tag 			:= "latest"
dockerfile 		:= "build/Dockerfile"
# Auto-detect host architecture for supported targets (amd64/arm64).
# For other architectures, set CAESIUM_PLATFORM explicitly to a supported value.
docker_arch     := if arch() == "aarch64" { "arm64" } else if arch() == "x86_64" { "amd64" } else { arch() }
platform        := env("CAESIUM_PLATFORM", "linux/" + docker_arch)
target_arch     := if platform == "linux/arm64" { "arm64" } else if platform == "linux/amd64" { "amd64" } else { docker_arch }
bld_dir         := "/bld/caesium"
repo_dir        := `pwd`
it_container    := "caesium-server-test"
# Podman support: set CAESIUM_PODMAN=true to prefix images with localhost/
podman          := env("CAESIUM_PODMAN", "false")
image_ref       := if podman == "true" { "localhost/" + repo + "/" + image } else { repo + "/" + image }
builder_ref     := if podman == "true" { "localhost/" + repo + "/" + builder_image } else { repo + "/" + builder_image }
sock            := env("CAESIUM_SOCK", "/var/run/docker.sock")
port            := env("CAESIUM_PORT", "8080")

validate-platform:
    @if [ "{{platform}}" != "linux/amd64" ] && [ "{{platform}}" != "linux/arm64" ]; then \
      echo "Unsupported CAESIUM_PLATFORM '{{platform}}' (supported: linux/amd64, linux/arm64)"; \
      exit 1; \
    fi

builder: validate-platform
    @if docker image inspect {{builder_ref}}:{{tag}} >/dev/null 2>&1; then \
        echo "Builder image {{builder_ref}}:{{tag}} already exists, skipping build."; \
    else \
        docker build --platform {{platform}} \
            --build-arg TARGETARCH={{target_arch}} \
            --target builder \
            -t {{builder_ref}}:{{tag}} \
            -f {{dockerfile}}.build .; \
    fi

builder-full: validate-platform
    @if docker image inspect {{builder_ref}}:{{tag}}-full >/dev/null 2>&1; then \
        echo "Builder image {{builder_ref}}:{{tag}}-full already exists, skipping build."; \
    else \
        docker build --platform {{platform}} \
            --build-arg TARGETARCH={{target_arch}} \
            --target builder-full \
            -t {{builder_ref}}:{{tag}}-full \
            -f {{dockerfile}}.build .; \
    fi

build: builder
    docker build --platform {{platform}} \
        --build-arg BUILDER_TAG={{tag}} \
        --target release \
        -t {{image_ref}}:{{tag}} \
        -f {{dockerfile}} .

# Build for a specific platform (requires buildx + QEMU for cross-platform)
build-cross target_platform:
    docker buildx build --platform {{target_platform}} \
        --target builder \
        -t {{builder_ref}}:{{tag}} \
        -f {{dockerfile}}.build --load .
    docker buildx build --platform {{target_platform}} \
        --build-arg BUILDER_TAG={{tag}} \
        --target release \
        -t {{image_ref}}:{{tag}} \
        -f {{dockerfile}} --load .

# Build and push multi-arch images for both builder and runtime
build-multiarch:
    docker buildx build --platform linux/amd64,linux/arm64 \
        --target builder \
        -t {{builder_ref}}:{{tag}} \
        -f {{dockerfile}}.build --push .
    docker buildx build --platform linux/amd64,linux/arm64 \
        --build-arg BUILDER_TAG={{tag}} \
        --target release \
        -t {{image_ref}}:{{tag}} \
        -f {{dockerfile}} --push .

build-release: builder
    docker build --platform {{platform}} \
        --build-arg BUILDER_TAG={{tag}} \
        --target release \
        -t {{image_ref}}:{{tag}} \
        -f {{dockerfile}} .

build-test: builder
    docker build --platform {{platform}} \
        --build-arg BUILDER_TAG={{tag}} \
        --target test \
        -t {{image_ref}}:{{tag}}-test \
        -f {{dockerfile}} .

push:
	docker push {{image_ref}}:{{tag}}

push-test:
	docker push {{image_ref}}:{{tag}}-test

push-multiarch:
    docker push {{image_ref}}:{{tag}}-amd64
    docker push {{image_ref}}:{{tag}}-arm64
    docker manifest create {{image_ref}}:{{tag}} \
        {{image_ref}}:{{tag}}-amd64 \
        {{image_ref}}:{{tag}}-arm64
    docker manifest push {{image_ref}}:{{tag}}

unit-test: builder-full
    docker run --rm --platform {{platform}} \
        -v {{repo_dir}}:{{bld_dir}} \
        -w {{bld_dir}} \
        {{builder_ref}}:{{tag}}-full \
        sh -c 'mkdir -p ui/dist && touch ui/dist/index.html && go test -race -coverprofile=coverage.txt -covermode=atomic -v ./...'

run: build
    docker run --platform {{platform}} \
        -d --name caesium-server \
        -p {{port}}:8080 \
        -v {{sock}}:/var/run/docker.sock \
        -e DOCKER_HOST=unix:///var/run/docker.sock \
        --user 0:0 \
        {{image_ref}}:{{tag}} start

rm:
	docker rm -f caesium-server

integration-test:
    docker build --platform {{platform}} \
        --build-arg BUILDER_TAG={{tag}} \
        --target test \
        -t {{repo}}/{{image}}:{{tag}}-test \
        -f {{dockerfile}} .
    docker rm -f {{it_container}} >/dev/null 2>&1 || true
    docker run -d --platform {{platform}} \
        --name {{it_container}} \
        --privileged \
        -v /var/run/docker.sock:/var/run/docker.sock \
        -e DOCKER_HOST=unix:///var/run/docker.sock \
        --user 0:0 \
        -e CAESIUM_LOG_LEVEL=debug \
        {{repo}}/{{image}}:{{tag}}-test start
    @cli_dir={{repo_dir}}/.tmp/caesium-cli; \
    rm -rf "$cli_dir"; \
    mkdir -p "$cli_dir"; \
    cli_ctr=$(docker create --platform {{platform}} {{repo}}/{{image}}:{{tag}}-test true); \
    trap 'docker rm -f "$cli_ctr" >/dev/null 2>&1 || true; rm -rf "$cli_dir"' EXIT; \
    docker cp "$cli_ctr":/bin/caesium "$cli_dir/caesium"; \
    chmod +x "$cli_dir/caesium"; \
    docker rm -f "$cli_ctr" >/dev/null 2>&1 || true; \
    if docker run --rm --platform {{platform}} \
        -v {{repo_dir}}:{{bld_dir}} \
        -v {{sock}}:/var/run/docker.sock \
        -e CAESIUM_CLI_PATH={{bld_dir}}/.tmp/caesium-cli/caesium \
        -e DOCKER_HOST=unix:///var/run/docker.sock \
        --network=container:{{it_container}} \
        -w {{bld_dir}} \
        {{builder_ref}}:{{tag}}-full \
        sh -c 'mkdir -p ui/dist && touch ui/dist/index.html && go test ./test/ -tags=integration'; then \
      docker rm -f {{it_container}} >/dev/null 2>&1 || true; \
    else \
      echo "integration tests failed; caesium server logs:"; \
      docker logs {{it_container}} || true; \
      docker rm -f {{it_container}} >/dev/null 2>&1 || true; \
      exit 1; \
    fi

integration-down:
    docker rm -f {{it_container}}

# Run integration tests against a Caesium server using the Podman engine.
# Requires Podman to be installed and the Podman socket to be active
# (run: systemctl --user enable --now podman.socket).
integration-test-podman: build
    #!/usr/bin/env bash
    set -euo pipefail
    PODMAN_SOCK="/run/user/$(id -u)/podman/podman.sock"
    docker rm -f caesium-server-podman >/dev/null 2>&1 || true
    docker run -d --name caesium-server-podman \
        --platform {{platform}} \
        -p 8080:8080 \
        -v "${PODMAN_SOCK}:/run/podman/podman.sock" \
        -e CAESIUM_PODMAN_URI=unix:///run/podman/podman.sock \
        -e CAESIUM_LOG_LEVEL=debug \
        --user 0:0 \
        {{repo}}/{{image}}:{{tag}} start
    if docker run --rm --platform {{platform}} \
        -v {{repo_dir}}:{{bld_dir}} \
        -e CAESIUM_TEST_ENGINE=podman \
        --network=host \
        -w {{bld_dir}} \
        {{repo}}/{{builder_image}}:{{tag}}-full \
        sh -c 'mkdir -p ui/dist && touch ui/dist/index.html && go test ./test/ -tags=integration -timeout 10m'; then
      docker rm -f caesium-server-podman >/dev/null 2>&1 || true
    else
      echo "integration tests (podman) failed; caesium server logs:"
      docker logs caesium-server-podman || true
      docker rm -f caesium-server-podman >/dev/null 2>&1 || true
      exit 1
    fi

hydrate:
    docker run --platform {{platform}} \
        --rm \
        --network=host \
        -v {{repo_dir}}/docs/examples:/examples:ro \
        {{image_ref}}:{{tag}} job apply --server http://127.0.0.1:{{port}} --path /examples

integration-up:
    docker build --platform {{platform}} \
        --build-arg BUILDER_TAG={{tag}} \
        --target test \
        -t {{repo}}/{{image}}:{{tag}}-test \
        -f {{dockerfile}} .
    docker rm -f {{it_container}} >/dev/null 2>&1 || true
    docker run -d --platform {{platform}} \
        --name {{it_container}} \
        --privileged \
        -v {{sock}}:/var/run/docker.sock \
        -e DOCKER_HOST=unix:///var/run/docker.sock \
        --user 0:0 \
        -e CAESIUM_LOG_LEVEL=debug \
        {{image_ref}}:{{tag}}-test start

lint: builder-full
    docker run --rm --platform {{platform}} \
        -v {{repo_dir}}:{{bld_dir}} \
        -w {{bld_dir}} \
        -e GOFLAGS=-buildvcs=false \
        {{builder_ref}}:{{tag}}-full \
        sh -c 'mkdir -p ui/dist && touch ui/dist/index.html && set -euo pipefail; \
            go fmt .; \
            go vet ./...; \
            golangci-lint run ./...'

ui-lint: builder-full
    docker run --rm --platform {{platform}} \
        -v {{repo_dir}}:{{bld_dir}} \
        -w {{bld_dir}}/ui \
        {{builder_ref}}:{{tag}}-full \
        sh -c 'npm ci --prefer-offline && npm run lint'

ui-test: builder-full
    docker run --rm --platform {{platform}} \
        -v {{repo_dir}}:{{bld_dir}} \
        -w {{bld_dir}}/ui \
        {{builder_ref}}:{{tag}}-full \
        sh -c 'npm ci --prefer-offline && npm test && npm run build:ci'

ui-e2e: build-release
    @docker rm -f caesium-server >/dev/null 2>&1 || true
    bash -lc 'set -euo pipefail; \
        trap "docker rm -f caesium-server >/dev/null 2>&1 || true" EXIT; \
        docker run --platform {{platform}} -d --name caesium-server -p {{port}}:8080 \
            -v {{sock}}:/var/run/docker.sock \
            -e DOCKER_HOST=unix:///var/run/docker.sock \
            --user 0:0 {{image_ref}}:{{tag}} start >/dev/null; \
        tries=0; \
        until curl -sf http://127.0.0.1:{{port}}/health >/dev/null; do \
            tries=$$((tries + 1)); \
            if [ "$$tries" -gt 30 ]; then \
                echo "Caesium did not become ready in time" >&2; \
                docker logs caesium-server >&2 || true; \
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
