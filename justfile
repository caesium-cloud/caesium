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

validate-platform:
    @if [ "{{platform}}" != "linux/amd64" ] && [ "{{platform}}" != "linux/arm64" ]; then \
      echo "Unsupported CAESIUM_PLATFORM '{{platform}}' (supported: linux/amd64, linux/arm64)"; \
      exit 1; \
    fi

builder: validate-platform
    docker build --platform {{platform}} \
        --build-arg TARGETARCH={{target_arch}} \
        -t {{repo}}/{{builder_image}}:{{tag}} \
        -f {{dockerfile}}.build .

build: builder
    docker build --platform {{platform}} \
        --build-arg BUILDER_TAG={{tag}} \
        --target release \
        -t {{repo}}/{{image}}:{{tag}} \
        -f {{dockerfile}} .

# Build for a specific platform (requires buildx + QEMU for cross-platform)
build-cross target_platform:
    docker buildx build --platform {{target_platform}} \
        -t {{repo}}/{{builder_image}}:{{tag}} \
        -f {{dockerfile}}.build --load .
    docker buildx build --platform {{target_platform}} \
        --build-arg BUILDER_TAG={{tag}} \
        --target release \
        -t {{repo}}/{{image}}:{{tag}} \
        -f {{dockerfile}} --load .

# Build and push multi-arch images for both builder and runtime
build-multiarch:
    docker buildx build --platform linux/amd64,linux/arm64 \
        -t {{repo}}/{{builder_image}}:{{tag}} \
        -f {{dockerfile}}.build --push .
    docker buildx build --platform linux/amd64,linux/arm64 \
        --build-arg BUILDER_TAG={{tag}} \
        --target release \
        -t {{repo}}/{{image}}:{{tag}} \
        -f {{dockerfile}} --push .

build-release: builder
    docker build --platform {{platform}} \
        --build-arg BUILDER_TAG={{tag}} \
        --target release \
        -t {{repo}}/{{image}}:{{tag}} \
        -f {{dockerfile}} .

build-test: builder
    docker build --platform {{platform}} \
        --build-arg BUILDER_TAG={{tag}} \
        --target test \
        -t {{repo}}/{{image}}:{{tag}}-test \
        -f {{dockerfile}} .

push:
	docker push {{repo}}/{{image}}:{{tag}}

push-test:
	docker push {{repo}}/{{image}}:{{tag}}-test

push-multiarch:
    docker push {{repo}}/{{image}}:{{tag}}-amd64
    docker push {{repo}}/{{image}}:{{tag}}-arm64
    docker manifest create {{repo}}/{{image}}:{{tag}} \
        {{repo}}/{{image}}:{{tag}}-amd64 \
        {{repo}}/{{image}}:{{tag}}-arm64
    docker manifest push {{repo}}/{{image}}:{{tag}}

unit-test: builder
    docker run --rm --platform {{platform}} \
        -v {{repo_dir}}:{{bld_dir}} \
        -w {{bld_dir}} \
        {{repo}}/{{builder_image}}:{{tag}} \
        go test -race -coverprofile=coverage.txt -covermode=atomic -v ./...

run: build
    docker run --platform {{platform}} \
        -d --name caesium-server \
        --network=host \
        -v /var/run/docker.sock:/var/run/docker.sock \
        -e DOCKER_HOST=unix:///var/run/docker.sock \
        --user 0:0 \
        {{repo}}/{{image}}:{{tag}} start

rm:
	docker rm -f caesium-server

integration-test:
    just integration-up
    if docker run --rm --platform {{platform}} \
        -v {{repo_dir}}:{{bld_dir}} \
        --network=container:{{it_container}} \
        -w {{bld_dir}} \
        {{repo}}/{{builder_image}}:{{tag}} \
        sh -c 'go test ./test/ -tags=integration'; then \
      docker rm -f {{it_container}} >/dev/null 2>&1 || true; \
    else \
      echo "integration tests failed; caesium server logs:"; \
      docker logs {{it_container}} || true; \
      docker rm -f {{it_container}} >/dev/null 2>&1 || true; \
      exit 1; \
    fi

console: build
    docker run --platform {{platform}} \
        -e TERM=xterm-256color \
        -e CAESIUM_HOST \
        -e CAESIUM_BASE_URL \
        -it --rm --name caesium-console \
        --network=host \
        {{repo}}/{{image}}:{{tag}} console

console-integration:
    docker run --platform {{platform}} \
        -e TERM=xterm-256color \
        -it --rm --name caesium-console \
        --network=container:{{it_container}} \
        {{repo}}/{{image}}:{{tag}} console

hydrate:
    docker run --platform {{platform}} \
        --rm \
        --network=host \
        -v {{repo_dir}}/docs/examples:/examples:ro \
        {{repo}}/{{image}}:{{tag}} job apply --server http://127.0.0.1:8080 --path /examples

integration-up: build-test
    docker rm -f {{it_container}} >/dev/null 2>&1 || true
    docker run -d --platform {{platform}} \
        --name {{it_container}} \
        --privileged \
        -v /var/run/docker.sock:/var/run/docker.sock \
        -e DOCKER_HOST=unix:///var/run/docker.sock \
        --user 0:0 \
        -e CAESIUM_LOG_LEVEL=debug \
        {{repo}}/{{image}}:{{tag}}-test start

integration-down:
    docker rm -f {{it_container}}

lint: builder
    docker run --rm --platform {{platform}} \
        -v {{repo_dir}}:{{bld_dir}} \
        -w {{bld_dir}} \
        -e GOFLAGS=-buildvcs=false \
        {{repo}}/{{builder_image}}:{{tag}} \
        sh -c 'set -euo pipefail; \
            go fmt .; \
            go vet ./...; \
            golangci-lint run ./...'
