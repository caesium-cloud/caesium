ARG BUILDER_TAG
FROM caesiumcloud/caesium-builder:${BUILDER_TAG} AS builder

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
