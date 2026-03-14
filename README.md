<p align="center">
  <img src="brand/caesium-icon.svg" width="120" alt="Caesium logo" />
</p>

<h1 align="center">caesium</h1>

<p align="center">
  <strong>Open-source distributed scheduling system</strong>
</p>

<p align="center">
  <a href="https://pkg.go.dev/github.com/caesium-cloud/caesium"><img src="https://pkg.go.dev/badge/github.com/caesium-cloud/caesium.svg" alt="Go Reference"></a>
  <a href="https://cloud.drone.io/caesium-cloud/caesium"><img src="https://cloud.drone.io/api/badges/caesium-cloud/caesium/status.svg" alt="Build Status"></a>
  <a href="https://goreportcard.com/report/github.com/caesium-cloud/caesium"><img src="https://goreportcard.com/badge/github.com/caesium-cloud/caesium" alt="Go Report Card"></a>
  <a href="https://codecov.io/gh/caesium-cloud/caesium"><img src="https://codecov.io/gh/caesium-cloud/caesium/branch/develop/graph/badge.svg?token=YXM50NU5GI" alt="Coverage"></a>
  <a href="https://github.com/caesium-cloud/caesium/releases"><img src="https://img.shields.io/github/release/caesium-cloud/caesium.svg" alt="Release"></a>
  <a href="https://hub.docker.com/r/caesiumcloud/caesium/"><img src="https://img.shields.io/docker/pulls/caesiumcloud/caesium?style=plastic" alt="Docker Pulls"></a>
</p>

----

Caesium is an open-source distributed scheduling system — named after the element whose atoms define the second. Currently a work in progress.

## Supported Architectures

- `linux/amd64`
- `linux/arm64`

Runtime images are published as multi-arch Docker manifests so `docker pull caesiumcloud/caesium:<tag>` resolves to the native architecture automatically.

## Building by Architecture

- Host-default platform (auto-detected): `just build`
- Override target platform: `CAESIUM_PLATFORM=linux/arm64 just build`
- Cross-build one platform with buildx: `just build-cross linux/arm64`
- Build and push multi-arch images: `just build-multiarch tag=<tag>`

## Mixed-Arch Cluster Notes

- Mixed `amd64`/`arm64` caesium clusters are supported when all nodes run compatible versions.
- Task container images must support the architecture of the node that runs them.
- On Docker/Kubernetes/Podman, multi-arch task images are pulled automatically when manifests include both architectures.

## Documentation

- Docs index: [docs/README.md](docs/README.md)
- Console guide: [docs/console.md](docs/console.md)
- Kubernetes + Helm guide: [docs/kubernetes-deployment.md](docs/kubernetes-deployment.md)
- Job manifest guide: [docs/job-definitions.md](docs/job-definitions.md)

