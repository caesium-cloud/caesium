# Shared BuildKit graph for CI product images. Baking `product` compiles the
# Go binary + UI once and fans the result into the release, test, and
# cli-static stages. Baking `reagents` compiles the four role binaries once.

variable "BUILDER_IMAGE" {
  default = "caesiumcloud/caesium-builder:latest"
}

variable "IMAGE_TAG" {
  default = "latest"
}

variable "TF_DIST" {
  default = "terraform"
}

variable "TF_VERSION" {
  default = "1.15.9"
}

target "release" {
  context    = "."
  dockerfile = "build/Dockerfile"
  target     = "release"
  args = {
    BUILDER_IMAGE = BUILDER_IMAGE
  }
  tags = ["caesiumcloud/caesium:${IMAGE_TAG}"]
}

target "test" {
  inherits = ["release"]
  target   = "test"
  tags     = ["caesiumcloud/caesium:${IMAGE_TAG}-test"]
}

target "cli-static" {
  inherits = ["release"]
  target   = "cli-static"
  tags     = ["caesium-cli-static:${IMAGE_TAG}"]
}

group "product" {
  targets = ["release", "test", "cli-static", "integration-runner"]
}

# Independent of the product compile, but shared by all integration lanes on
# this architecture. The runner contains no Go compiler or module cache.
target "integration-runner" {
  context = "."
  dockerfile = "build/Dockerfile.integration"
  target = "integration-runner"
  args = { BUILDER_IMAGE = BUILDER_IMAGE }
  tags = ["caesiumcloud/caesium-integration:${IMAGE_TAG}"]
}

target "reagent-git-source" {
  context    = "."
  dockerfile = "build/Dockerfile.reagents"
  target     = "git-source"
  args = {
    TF_DIST    = TF_DIST
    TF_VERSION = TF_VERSION
  }
  tags = [
    "caesiumcloud/git-source:${IMAGE_TAG}",
    "caesiumcloud/git-source:latest",
  ]
}

target "reagent-tf-discover" {
  inherits = ["reagent-git-source"]
  target   = "tf-discover"
  tags = [
    "caesiumcloud/tf-discover:${IMAGE_TAG}",
    "caesiumcloud/tf-discover:latest",
  ]
}

target "reagent-tf-warm" {
  inherits = ["reagent-git-source"]
  target   = "tf-warm"
  tags = [
    "caesiumcloud/tf-warm:${IMAGE_TAG}",
    "caesiumcloud/tf-warm:latest",
  ]
}

target "reagent-tf-runner" {
  inherits = ["reagent-git-source"]
  target   = "tf-runner"
  tags = [
    "caesiumcloud/tf-runner:${IMAGE_TAG}",
    "caesiumcloud/tf-runner:latest",
  ]
}

target "reagent-toolchain" {
  inherits = ["reagent-git-source"]
  target   = "toolchain"
  tags     = ["caesiumcloud/caesium-reagent-toolchain:${IMAGE_TAG}"]
}

group "reagents" {
  targets = [
    "reagent-git-source",
    "reagent-tf-discover",
    "reagent-tf-warm",
    "reagent-tf-runner",
    "reagent-toolchain",
  ]
}

# The arm64 integration/publish lanes consume only runtime roles. They do not
# run reagent lint/tests, so avoid compiling and transferring that toolchain.
group "reagent-roles" {
  targets = [
    "reagent-git-source",
    "reagent-tf-discover",
    "reagent-tf-warm",
    "reagent-tf-runner",
  ]
}
