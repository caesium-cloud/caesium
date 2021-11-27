ARG BUILDER_TAG
FROM caesiumcloud/caesium-builder:${BUILDER_TAG} as builder

# Package for lightweight deployment
FROM alpine
COPY --chown=0:0 --from=builder /dist /bin/
ENTRYPOINT ["/bin/caesium"]