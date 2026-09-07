# Accept a full builder image reference so local Podman images can be addressed via localhost/.
ARG BUILDER_IMAGE=caesiumcloud/caesium-builder:latest
FROM ${BUILDER_IMAGE} AS builder

WORKDIR /bld/caesium

# 1. Install npm deps (only re-runs when package files change)
COPY ui/package.json ui/package-lock.json ./ui/
RUN --mount=type=cache,target=/root/.npm \
    cd ui && npm ci

# 2. Copy UI source and build (only re-runs when ui/ source changes)
COPY ui/ ./ui/
RUN cd ui && npm run build

# 3. Copy all source and build Go binary
#    ui/node_modules and ui/dist are in .dockerignore so COPY won't clobber them.
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go build -trimpath -ldflags="-s -w" -o /dist/bin/caesium ./

# Collect dynamically linked libraries
WORKDIR /dist
RUN ldd /dist/bin/caesium | tr -s '[:blank:]' '\n' | grep '^/' | \
    xargs -I % sh -c 'mkdir -p $(dirname ./%); cp % ./%;'

# -------- Static CLI binary (bare-host portable, Linux only) --------
# `docker build --target cli-static -f build/Dockerfile .` produces an image
# whose /cli/caesium is a *statically* linked executable: it runs on any Linux
# host of the same architecture with no shared libraries at all, which is what
# the GitHub Release artifacts (caesium-linux-amd64 / caesium-linux-arm64) ship.
#
# It reuses the `builder` stage above, so the UI bundle and the Go module cache
# are already in place and only the link differs. Nothing below this line is
# reachable from `--target release` or `--target test`: the image build is
# unchanged.
#
# Linux only: the toolchain is CGO against musl + a dqlite built in-stage, so
# there is no darwin target here (macOS users run the container - `just cli`).
FROM builder AS cli-static
# The builder stage ends in /dist (it collects the .so closure there).
WORKDIR /bld/caesium

# Static counterparts of the shared libraries the dynamic build links.
# libdqlite.a comes from the builder image itself (build/Dockerfile.build's
# dqlite stage configures with --enable-static).
RUN apk add --no-cache \
    sqlite-static libuv-static lz4-static \
    zlib-static openssl-libs-static \
    file

# Build tags:
#   libsqlite3               mattn/go-sqlite3 links the system libsqlite3
#                            (sqlite-static) instead of compiling the
#                            amalgamation - the form go-dqlite documents for
#                            static builds.
#   containers_image_openpgp containers/image (pulled in by podman/v5) uses its
#                            pure-Go OpenPGP implementation instead of libgpgme.
#                            Required: Alpine ships no gpgme-static, and
#                            libgpgme.a alone does not resolve (undefined
#                            `gpgrt_lock_lock`, `gpg_err_code_from_syserror` -
#                            the dynamic build gets those transitively through
#                            libgpgme.so's DT_NEEDED). Only container-image
#                            *signature verification* differs, which caesium
#                            does not use.
# -luv/-llz4/-lsqlite3 are appended explicitly: go-dqlite's cgo directives only
# pass -ldqlite and a static libdqlite.a has no DT_NEEDED to pull them in.
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go build -tags "libsqlite3,containers_image_openpgp" -trimpath \
        -ldflags="-s -w -linkmode external -extldflags '-static -luv -llz4 -lsqlite3 -lm'" \
        -o /cli/caesium ./

# Fail the stage unless the result really is static, so a toolchain change can
# never silently republish a dynamically linked "portable" binary. `ldd` on a
# static musl binary prints "Not a valid dynamic program" (no `=>` lines) and
# `file` reports "statically linked"; both are asserted.
RUN ! ldd /cli/caesium 2>/dev/null | grep -q '=>' && \
    file /cli/caesium | grep -q 'statically linked' && \
    file /cli/caesium && \
    /cli/caesium --help >/dev/null && \
    echo "OK: /cli/caesium is statically linked"

# -------- Release image (no DinD) --------
FROM alpine:3.23 AS release
RUN apk add --no-cache ca-certificates libuv lz4-libs sqlite-libs tzdata && \
    adduser -D -H -u 10001 caesium && \
    mkdir -p /var/lib/caesium/dqlite && \
    chown 10001:10001 /var/lib/caesium/dqlite
COPY --chown=0:0 --from=builder /dist/ /
USER 10001:10001
ENTRYPOINT ["/bin/caesium"]

# -------- Test image (DinD for integration) --------
FROM docker:29.2.0-alpine3.23 AS test
RUN apk add --no-cache libuv lz4-libs sqlite-libs tzdata && \
    mkdir -p /var/lib/caesium/dqlite && \
    chown 10001:10001 /var/lib/caesium/dqlite
COPY --chown=0:0 --from=builder /dist/ /

# Wrapper to start dockerd (DinD) and then the app
RUN printf '#!/bin/sh\nset -e\n\n# start docker daemon in background\n dockerd-entrypoint.sh dockerd &\n\n# wait for docker to be ready\ntries=0\nuntil docker info >/dev/null 2>&1; do\n  tries=$((tries+1))\n  if [ "$tries" -gt 60 ]; then\n    echo "Docker daemon did not become ready in time" >&2\n    exit 1\n  fi\n  sleep 1\ndone\n\n# run caesium\nexec /bin/caesium "$@"\n' > /usr/local/bin/start-caesium.sh && chmod +x /usr/local/bin/start-caesium.sh

ENTRYPOINT ["/usr/local/bin/start-caesium.sh"]
