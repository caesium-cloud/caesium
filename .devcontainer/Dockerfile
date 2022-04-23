# See here for image contents: https://github.com/microsoft/vscode-dev-containers/tree/v0.224.3/containers/alpine/.devcontainer/base.Dockerfile

# [Choice] Alpine version: 3.14, 3.13, 3.12, 3.11
ARG VARIANT="3.15"
FROM mcr.microsoft.com/vscode/devcontainers/base:0-alpine-${VARIANT}

COPY --from=golang:1.18-alpine /usr/local/go/ /usr/local/go/
 
ENV PATH="/usr/local/go/bin:${PATH}"

# Update & install OS dependencies
WORKDIR /bld
RUN apk update 
RUN apk add --no-cache ca-certificates git \
    dumb-init autoconf automake libtool libuv-dev \
    lz4-dev make gcc musl-dev linux-headers g++ \
    gpgme-dev btrfs-progs-dev lvm2-dev

# Compile & install
RUN git clone https://github.com/canonical/raft 
WORKDIR /bld/raft
RUN autoreconf -i && \
    ./configure --enable-uv --prefix=/usr && \
    make -j4 install
WORKDIR /bld
RUN apk add sqlite-dev
RUN git clone https://github.com/canonical/dqlite
WORKDIR /bld/dqlite
RUN autoreconf -i && ./configure --prefix=/usr && make -j4 install

# Go build environment & dependencies
WORKDIR /workspaces/caesium
ENV CGO_ENABLED=1 \
    CGO_LDFLAGS_ALLOW="-Wl,-z,now" \
    GOOS=linux \
    GOARCH=amd64
