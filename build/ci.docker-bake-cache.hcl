# CI-only override: use the builder's cached graph as a named context instead
# of transferring its full Docker image into downstream build jobs.
variable "CACHE_ARCH" { default = "amd64" }

target "ci-builder" {
  context = "."
  dockerfile = "build/Dockerfile.build"
  target = "builder"
  args = { TARGETARCH = CACHE_ARCH }
  cache-from = ["type=gha,scope=builder-linux-${CACHE_ARCH}"]
}

target "release" {
  args = { BUILDER_IMAGE = "ci-builder" }
  contexts = { ci-builder = "target:ci-builder" }
  cache-from = ["type=gha,scope=product-release-${CACHE_ARCH}"]
  cache-to = ["type=gha,scope=product-release-${CACHE_ARCH},mode=max,ignore-error=true"]
}

target "test" {
  cache-from = ["type=gha,scope=product-test-${CACHE_ARCH}"]
  cache-to = ["type=gha,scope=product-test-${CACHE_ARCH},mode=max,ignore-error=true"]
}

target "cli-static" {
  cache-from = ["type=gha,scope=product-cli-${CACHE_ARCH}"]
  cache-to = ["type=gha,scope=product-cli-${CACHE_ARCH},mode=max,ignore-error=true"]
}

target "integration-runner" {
  args = { BUILDER_IMAGE = "ci-builder" }
  contexts = { ci-builder = "target:ci-builder" }
  cache-from = ["type=gha,scope=integration-runner-${CACHE_ARCH}"]
  cache-to = ["type=gha,scope=integration-runner-${CACHE_ARCH},mode=max,ignore-error=true"]
}

target "reagent-git-source" {
  cache-from = ["type=gha,scope=reagent-git-source-${CACHE_ARCH}"]
  cache-to = ["type=gha,scope=reagent-git-source-${CACHE_ARCH},mode=max,ignore-error=true"]
}

target "reagent-tf-discover" {
  cache-from = ["type=gha,scope=reagent-tf-discover-${CACHE_ARCH}"]
  cache-to = ["type=gha,scope=reagent-tf-discover-${CACHE_ARCH},mode=max,ignore-error=true"]
}

target "reagent-tf-warm" {
  cache-from = ["type=gha,scope=reagent-tf-warm-${CACHE_ARCH}"]
  cache-to = ["type=gha,scope=reagent-tf-warm-${CACHE_ARCH},mode=max,ignore-error=true"]
}

target "reagent-tf-runner" {
  cache-from = ["type=gha,scope=reagent-tf-runner-${CACHE_ARCH}"]
  cache-to = ["type=gha,scope=reagent-tf-runner-${CACHE_ARCH},mode=max,ignore-error=true"]
}

target "reagent-toolchain" {
  cache-from = ["type=gha,scope=reagent-toolchain-${CACHE_ARCH}"]
  cache-to = ["type=gha,scope=reagent-toolchain-${CACHE_ARCH},mode=max,ignore-error=true"]
}
