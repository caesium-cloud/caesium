REPO 			?= caesiumcloud
IMAGE			?= caesium
BUILDER_IMAGE	?= caesium-builder
TAG 			?= latest
DOCKERFILE 		?= build/Dockerfile

builder:
	docker build -t ${REPO}/${BUILDER_IMAGE}:${TAG} \
		-f ${DOCKERFILE}.build .

build: builder
	docker build -t ${REPO}/${IMAGE}:${TAG} \
		--build-arg BUILDER_TAG=${TAG} \
		-f ${DOCKERFILE} .

push:
	docker push ${REPO}/${IMAGE}:${TAG}

unit-test: builder
	docker run --rm -v $(PWD):/bld/caesium \
		-v ${GOPATH}:/go \
		-w /bld/caesium \
		${REPO}/${BUILDER_IMAGE}:${TAG} \
		go test -race -covermode=atomic ./...

run: build
	docker run -d --name caesium \
		--network=host \
	      	${REPO}/${IMAGE}:${TAG} start

rm:
	docker rm -f caesium

integration-test: builder
	docker run --rm -v $(PWD):/bld/caesium \
		--network=host \
		-v ${GOPATH}:/go \
		-w /bld/caesium \
		${REPO}/${BUILDER_IMAGE}:${TAG} \
		go test ./test/ -tags=integration
