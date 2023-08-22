ARG BUILDER_TAG
FROM caesiumcloud/caesium-builder:${BUILDER_TAG} as builder

# Build binary
WORKDIR /bld/caesium
COPY . .
RUN go build

# Link for CGO
WORKDIR /dist
RUN cp /bld/caesium/caesium .
RUN ldd caesium | tr -s '[:blank:]' '\n' | grep '^/' | \
    xargs -I % sh -c 'mkdir -p $(dirname ./%); cp % ./%;'

# Package for lightweight deployment
FROM docker:24.0.5-dind-alpine3.18
RUN mkdir -p /opt/caesium/dqlite
COPY --chown=0:0 --from=builder /dist/caesium /bin/caesium
COPY --chown=0:0 --from=builder /dist/usr/lib/* /usr/lib/
COPY --chown=0:0 --from=builder /lib/* /lib/
ENTRYPOINT ["/bin/caesium"]