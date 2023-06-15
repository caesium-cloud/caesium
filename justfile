repo 			:= "caesiumcloud"
image			:= "caesium"
builder_image	:= image + "-builder"
tag 			:= "latest"
dockerfile 		:= "build/Dockerfile"
platform        := "linux/amd64"
gopath          := `go env GOPATH`

builder:
    docker build --platform {{platform}} \
        -t {{repo}}/{{builder_image}}:{{tag}} \
        -f {{dockerfile}}.build .

build: builder
    docker build --platform {{platform}} \
        --build-arg BUILDER_TAG={{tag}} \
        -t {{repo}}/{{image}}:{{tag}} \
        -f {{dockerfile}} .

push:
	docker push {{repo}}/{{image}}:{{tag}}

unit-test: builder
    docker run --rm --platform {{platform}} \
        -v $(PWD):/bld/caesium \
        -v {{gopath}}:/go \
        -w /bld/caesium \
        {{repo}}/{{builder_image}}:{{tag}} \
        go test -race -covermode=atomic ./...

run: build
    docker run --platform {{platform}} \
        -d --name caesium \
        --network=host \
        {{repo}}/{{image}}:{{tag}} start

rm:
	docker rm -f caesium

integration-test: build run && rm
    docker run --rm --platform {{platform}} \
        -v $(PWD):/bld/caesium \
        --network=host \
        -v {{gopath}}:/go \
        -w /bld/caesium \
        {{repo}}/{{builder_image}}:{{tag}} \
        go test ./test/ -tags=integration