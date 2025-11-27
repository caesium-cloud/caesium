repo 			:= "caesiumcloud"
image			:= "caesium"
builder_image	:= image + "-builder"
tag 			:= "latest"
dockerfile 		:= "build/Dockerfile"
platform        := "linux/amd64"
bld_dir         := "/bld/caesium"
repo_dir        := `pwd`
it_container    := "caesium-server-test"

builder:
    docker build --platform {{platform}} \
        -t {{repo}}/{{builder_image}}:{{tag}} \
        -f {{dockerfile}}.build .

build: builder
    docker build --platform {{platform}} \
        --build-arg BUILDER_TAG={{tag}} \
        --target release \
        -t {{repo}}/{{image}}:{{tag}} \
        -f {{dockerfile}} .

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
