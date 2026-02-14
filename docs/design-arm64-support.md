# Design: Comprehensive ARM64 Support

## Status

Implemented

Implemented by PR [#74](https://github.com/caesium-cloud/caesium/pull/74) (plus follow-up review fixes on that branch).

## Problem

Caesium currently only builds and tests for `linux/amd64`. The build toolchain — justfile, Dockerfile.build, and CircleCI pipeline — all hardcode x86-64 as the target platform. This prevents running caesium natively on ARM64 hardware (Apple Silicon Macs, AWS Graviton, Ampere Altra, Raspberry Pi clusters, etc.).

The Go application code itself is architecture-agnostic, but the **build infrastructure** and **CGO dependency chain** (dqlite, compiled from C source) are the blockers.

## Current State

### What's hardcoded to amd64

| File | Line(s) | Issue |
|---|---|---|
| `justfile` | 6 | `platform := "linux/amd64"` |
| `build/Dockerfile.build` | 25-26 | `GOOS=linux`, `GOARCH=amd64` |
| `.circleci/config.yml` | 6, 28, 49, 66 | `image: ubuntu-2404:current` (amd64-only machine executor) |

### What's already multi-arch compatible

| Component | Status | Notes |
|---|---|---|
| Go application code | Ready | No build tags, no assembly, no arch-specific logic |
| Container engines (Docker/K8s/Podman) | Ready | Pure Go client libraries, no arch assumptions |
| `alpine:3.22` base image | Ready | Published for amd64, arm64, arm/v7, etc. |
| `golang:1.25.6-alpine3.23` builder base | Ready | Multi-arch |
| Alpine packages (`libuv-dev`, `sqlite-dev`, etc.) | Ready | Available for all Alpine-supported architectures |
| `docker:29.2.0-alpine3.23` (test image) | Ready | Multi-arch |
| All Go dependencies except go-dqlite/go-sqlite3 | Ready | Pure Go |

### The CGO dependency chain

This is the critical path for ARM64 support:

```
caesium binary
  ├── github.com/canonical/go-dqlite/v3  (CGO, wraps libdqlite)
  │     └── libdqlite.so (C library, compiled from source in Dockerfile.build)
  │           ├── libuv (Alpine package, multi-arch)
  │           ├── lz4 (Alpine package, multi-arch)
  │           └── sqlite3 (Alpine package, multi-arch)
  └── github.com/mattn/go-sqlite3 (CGO, wraps libsqlite3)
        └── sqlite3 (Alpine package, multi-arch)
```

dqlite v1.18.5 bundles its own raft implementation (since v1.18.1), so there's no separate raft C library to compile. The dqlite `./configure && make install` in `Dockerfile.build:19` will work on ARM64 as-is — it uses autoconf which detects the host architecture. The only issue is that `GOARCH=amd64` is hardcoded, and the justfile forces `--platform linux/amd64`.

**In short: the application and all dependencies already support ARM64. We just need to parameterize the build and add CI coverage.**

## Design

### Phase 1: Multi-Arch Build Infrastructure

#### 1.1 — Parameterize the justfile

**File:** `justfile`

Replace the hardcoded platform with an overridable variable:

```just
platform := env("CAESIUM_PLATFORM", "linux/" + arch())
```

`arch()` is a just built-in that returns the host architecture (`aarch64` on ARM64, `x86_64` on amd64). For Docker's `--platform` flag, we need the Docker convention (`linux/amd64`, `linux/arm64`), so we'll add a helper:

```just
# Detect host docker platform
docker_arch := if arch() == "aarch64" { "arm64" } else { "amd64" }
platform := env("CAESIUM_PLATFORM", "linux/" + docker_arch)
```

This means:
- On an ARM64 Mac/Linux: defaults to `linux/arm64`
- On amd64: defaults to `linux/amd64`
- Overridable: `CAESIUM_PLATFORM=linux/amd64 just build`

No other justfile changes needed — all recipes already use `{{platform}}`.

#### 1.2 — Parameterize Dockerfile.build

**File:** `build/Dockerfile.build`

Replace hardcoded `GOARCH=amd64` with a build arg that auto-detects:

```dockerfile
FROM golang:1.25.6-alpine3.23

# ... (package install unchanged — Alpine packages are arch-native) ...

# Compile & install dqlite (autoconf detects host arch automatically)
WORKDIR /bld
ARG DQLITE_REF=v1.18.5
RUN git clone --depth 1 --branch ${DQLITE_REF} https://github.com/canonical/dqlite
WORKDIR /bld/dqlite
RUN autoreconf -i && \
    CFLAGS="-Wno-error=maybe-musttail-local-addr" \
    CXXFLAGS="-Wno-error=maybe-musttail-local-addr" \
    ./configure --disable-backtrace --prefix=/usr && \
    make -j$(nproc) install

# Go build environment
WORKDIR /bld/caesium
ARG TARGETARCH
ENV CGO_ENABLED=1 \
    CGO_LDFLAGS_ALLOW="-Wl,-z,now" \
    GOOS=linux \
    GOARCH=${TARGETARCH}
COPY go.mod go.sum ./
RUN go mod download
```

Key changes:
- `GOARCH=amd64` becomes `GOARCH=${TARGETARCH}`. Docker BuildKit automatically sets `TARGETARCH` to `amd64` or `arm64` based on the `--platform` flag.
- `make -j4` becomes `make -j$(nproc)` to use all available cores.
- dqlite compilation needs no changes — autoconf handles architecture detection.

#### 1.3 — Verify Dockerfile (release/test stages)

**File:** `build/Dockerfile`

The release and test stages are already multi-arch compatible:
- `alpine:3.22` and `docker:29.2.0-alpine3.23` are multi-arch.
- The `ldd` library collection (lines 12-14) runs in the build context so it collects the correct arch's libraries.
- No changes needed here.

#### 1.4 — Add `docker buildx` support for cross-compilation

For developers on amd64 who want to build ARM64 images (or vice versa), add a `build-cross` recipe:

**File:** `justfile`

```just
# Build for a specific target platform (requires buildx + QEMU)
build-cross target_platform:
    docker buildx build --platform {{target_platform}} \
        -t {{repo}}/{{builder_image}}:{{tag}} \
        -f {{dockerfile}}.build --load .
    docker buildx build --platform {{target_platform}} \
        --build-arg BUILDER_TAG={{tag}} \
        --target release \
        -t {{repo}}/{{image}}:{{tag}} \
        -f {{dockerfile}} --load .

# Build multi-arch manifest and push
build-multiarch:
    docker buildx build --platform linux/amd64,linux/arm64 \
        -t {{repo}}/{{builder_image}}:{{tag}} \
        -f {{dockerfile}}.build --push .
    docker buildx build --platform linux/amd64,linux/arm64 \
        --build-arg BUILDER_TAG={{tag}} \
        --target release \
        -t {{repo}}/{{image}}:{{tag}} \
        -f {{dockerfile}} --push .
```

**Note:** Cross-compiling dqlite (C code) via QEMU emulation will be slow but functional. For CI, native runners are preferred (see Phase 2).

### Phase 2: CI Pipeline — ARM64 Testing

#### 2.1 — Add ARM64 builder job

**File:** `.circleci/config.yml`

CircleCI supports ARM64 machine executors via the `arm.medium` resource class:

```yaml
  builder-arm64:
    machine:
      image: ubuntu-2404:current
      docker_layer_caching: true
    resource_class: arm.medium
    working_directory: /home/circleci/project
    steps:
      - checkout
      - run:
          name: Install just
          command: sudo apt-get update && sudo apt-get install -y just
      - run:
          name: Build builder image (arm64)
          command: just tag=$CIRCLE_SHA1-arm64 builder
      - run:
          name: Save builder image
          command: docker save caesiumcloud/caesium-builder:$CIRCLE_SHA1-arm64 | gzip > /tmp/builder-arm64.tar.gz
      - persist_to_workspace:
          root: /tmp
          paths:
            - builder-arm64.tar.gz
```

The only difference from the existing `builder` job is `resource_class: arm.medium`. Since we parameterized the build to auto-detect architecture, the same `just builder` command produces an ARM64 image on ARM64 hardware.

#### 2.2 — Add ARM64 unit test job

```yaml
  unit-test-arm64:
    machine:
      image: ubuntu-2404:current
    resource_class: arm.medium
    working_directory: /home/circleci/project
    steps:
      - checkout
      - attach_workspace:
          at: /tmp/workspace
      - run:
          name: Install just
          command: sudo apt-get update && sudo apt-get install -y just
      - run:
          name: Load builder image
          command: gunzip -c /tmp/workspace/builder-arm64.tar.gz | docker load
      - run:
          name: Unit tests (arm64)
          command: just tag=$CIRCLE_SHA1-arm64 unit-test
```

#### 2.3 — Add ARM64 integration test job

```yaml
  build-and-integration-test-arm64:
    machine:
      image: ubuntu-2404:current
    resource_class: arm.medium
    working_directory: /home/circleci/project
    steps:
      - checkout
      - attach_workspace:
          at: /tmp/workspace
      - run:
          name: Install just
          command: sudo apt-get update && sudo apt-get install -y just
      - run:
          name: Load builder image
          command: gunzip -c /tmp/workspace/builder-arm64.tar.gz | docker load
      - run:
          name: Build release image (arm64)
          command: just tag=$CIRCLE_SHA1-arm64 build
      - run:
          name: Integration tests (arm64)
          command: just tag=$CIRCLE_SHA1-arm64 integration-test
```

#### 2.4 — Update workflow

```yaml
workflows:
  ci:
    jobs:
      # amd64 pipeline (unchanged)
      - builder
      - lint:
          requires: [builder]
      - unit-test:
          requires: [builder]
      - build-and-integration-test:
          requires: [builder]

      # arm64 pipeline (parallel)
      - builder-arm64
      - unit-test-arm64:
          requires: [builder-arm64]
      - build-and-integration-test-arm64:
          requires: [builder-arm64]
```

The ARM64 pipeline runs in parallel with the amd64 pipeline — no added latency to the existing workflow. Lint only needs to run once (architecture-independent).

### Phase 3: Multi-Arch Image Publishing

#### 3.1 — Publish multi-arch Docker manifests

**File:** `.circleci/config.yml`

Add a `publish` job that creates a multi-arch manifest from the per-arch images:

```yaml
  publish:
    machine:
      image: ubuntu-2404:current
    resource_class: medium
    steps:
      - checkout
      - attach_workspace:
          at: /tmp/workspace
      - run:
          name: Install just
          command: sudo apt-get update && sudo apt-get install -y just
      - run:
          name: Load images
          command: |
            gunzip -c /tmp/workspace/release-amd64.tar.gz | docker load
            gunzip -c /tmp/workspace/release-arm64.tar.gz | docker load
      - run:
          name: Login to registry
          command: echo "$DOCKER_PASSWORD" | docker login -u "$DOCKER_USERNAME" --password-stdin
      - run:
          name: Push multi-arch manifest
          command: |
            docker push caesiumcloud/caesium:$CIRCLE_SHA1-amd64
            docker push caesiumcloud/caesium:$CIRCLE_SHA1-arm64
            docker manifest create caesiumcloud/caesium:$CIRCLE_SHA1 \
              caesiumcloud/caesium:$CIRCLE_SHA1-amd64 \
              caesiumcloud/caesium:$CIRCLE_SHA1-arm64
            docker manifest push caesiumcloud/caesium:$CIRCLE_SHA1
```

This means `docker pull caesiumcloud/caesium:<tag>` automatically pulls the correct architecture.

#### 3.2 — Update justfile push targets

**File:** `justfile`

```just
push-multiarch:
    docker manifest create {{repo}}/{{image}}:{{tag}} \
        {{repo}}/{{image}}:{{tag}}-amd64 \
        {{repo}}/{{image}}:{{tag}}-arm64
    docker manifest push {{repo}}/{{image}}:{{tag}}
```

### Phase 4: Validation and Documentation

#### 4.1 — Verify dqlite cluster operation on ARM64

dqlite's Raft protocol uses a binary wire format. Verify:
- Single-node ARM64 cluster starts and serves requests.
- Multi-node cluster with all ARM64 nodes works.
- **Mixed-arch clusters** (amd64 + arm64 nodes) — dqlite uses gRPC/protobuf internally, so this should work, but needs explicit testing.

#### 4.2 — Verify container engines on ARM64

The container engine code is architecture-agnostic (pure Go client libraries), but the **task container images** users run must be multi-arch. This isn't a caesium code change — it's a documentation concern:

- Docker: `docker pull` on ARM64 automatically pulls ARM64 images if available.
- Kubernetes: Pod scheduling on ARM64 nodes pulls ARM64 images.
- Podman: Same as Docker.

Document that users must ensure their task images support the target architecture.

#### 4.3 — Performance benchmarking

Run the integration test suite on both architectures and compare:
- dqlite read/write latency
- Job scheduling overhead
- Container create/start times

No code changes — just operational validation.

#### 4.4 — Documentation

Add to project README or docs:
- Supported architectures: `linux/amd64`, `linux/arm64`
- How to build for a specific architecture locally
- Mixed-arch cluster considerations
- CI badge showing ARM64 test status

## Implementation Plan

### Phase 1 — Multi-Arch Build Infrastructure

- [ ] **1.1** Parameterize `justfile` platform variable to auto-detect host architecture
- [ ] **1.2** Replace `GOARCH=amd64` in `build/Dockerfile.build` with `ARG TARGETARCH` / `GOARCH=${TARGETARCH}`
- [ ] **1.3** Change `make -j4` to `make -j$(nproc)` in `build/Dockerfile.build`
- [ ] **1.4** Verify `build/Dockerfile` release and test stages need no changes (they shouldn't)
- [ ] **1.5** Add `build-cross` and `build-multiarch` recipes to `justfile`
- [ ] **1.6** Test local build on ARM64 hardware (Apple Silicon or Graviton)
- [ ] **1.7** Test local build on amd64 to confirm no regression

### Phase 2 — CI ARM64 Test Coverage

- [ ] **2.1** Add `builder-arm64` job to `.circleci/config.yml` with `resource_class: arm.medium`
- [ ] **2.2** Add `unit-test-arm64` job
- [ ] **2.3** Add `build-and-integration-test-arm64` job
- [ ] **2.4** Update workflow to run ARM64 jobs in parallel with amd64 jobs
- [ ] **2.5** Verify ARM64 CI passes end-to-end (unit + integration)

### Phase 3 — Multi-Arch Publishing

- [ ] **3.1** Update amd64 and arm64 build jobs to persist release images with arch-suffixed tags
- [ ] **3.2** Add `publish` job that creates and pushes multi-arch Docker manifest
- [ ] **3.3** Add `push-multiarch` recipe to `justfile`
- [ ] **3.4** Wire publish job into workflow (on tag/release trigger)

### Phase 4 — Validation and Docs

- [ ] **4.1** Validate single-node dqlite on ARM64
- [ ] **4.2** Validate multi-node dqlite cluster on ARM64
- [ ] **4.3** Test mixed-arch cluster (amd64 + arm64 nodes)
- [ ] **4.4** Run performance benchmarks on both architectures
- [ ] **4.5** Document multi-arch support, build instructions, and mixed-cluster guidance

## Risks and Mitigations

| Risk | Likelihood | Mitigation |
|---|---|---|
| dqlite C compilation fails on ARM64 | Low | dqlite officially supports ARM64; autoconf handles arch detection. Test early in Phase 1. |
| QEMU cross-compilation too slow for CI | Medium | Use native ARM64 CI runners (`arm.medium`), not emulation. Cross-compile only for local dev convenience. |
| Mixed-arch dqlite cluster incompatibility | Low | dqlite uses protobuf wire format (arch-independent). Validate in Phase 4. |
| ARM64 CI runner availability/cost | Low | CircleCI `arm.medium` is generally available. Cost is comparable to amd64 `medium`. |
| User task images not available for ARM64 | N/A (not our problem) | Document that task container images must support the target architecture. |
| CGO compiler flag differences on ARM64 | Low | The `-Wno-error=maybe-musttail-local-addr` flag may not be needed on ARM64 (it's a GCC/clang codegen quirk). If it causes warnings, gate it with an arch check. |

## Scope and Non-Goals

**In scope:**
- Native ARM64 builds of caesium itself
- ARM64 CI test coverage (unit + integration)
- Multi-arch Docker image publishing
- Documentation

**Not in scope:**
- 32-bit ARM (arm/v7) — can be added later with the same approach
- Windows ARM64 — caesium targets Linux
- ARM64-specific performance optimizations — the Go compiler handles this
- Ensuring user-provided task images are multi-arch — that's the user's responsibility
