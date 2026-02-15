# Caesium

[![Pkg Widget]][Pkg]
[![Drone CI Widget]][Drone CI]
[![Go Report Widget]][Go Report]
[![Codecov Widget]][Codecov]
[![GitHub Widget]][GitHub]
[![Docker Widget]][Docker]

----

Caesium is an open source distributed scheduling system that is currently a work in progress.

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

[Pkg]: https://pkg.go.dev/github.com/caesium-cloud/caesium
[Pkg Widget]: https://pkg.go.dev/badge/github.com/caesium-cloud/caesium.svg
[Drone CI]: https://cloud.drone.io/caesium-cloud/caesium
[Drone CI Widget]: https://cloud.drone.io/api/badges/caesium-cloud/caesium/status.svg
[Go Report]: https://goreportcard.com/report/github.com/caesium-cloud/caesium
[Go Report Widget]: https://goreportcard.com/badge/github.com/caesium-cloud/caesium
[Codecov]: https://codecov.io/gh/caesium-cloud/caesium
[Codecov Widget]: https://codecov.io/gh/caesium-cloud/caesium/branch/develop/graph/badge.svg?token=YXM50NU5GI
[GitHub]: https://github.com/caesium-cloud/caesium/releases
[GitHub Widget]: https://img.shields.io/github/release/caesium-cloud/caesium.svg
[Docker]: https://hub.docker.com/r/caesiumcloud/caesium/
[Docker Widget]: https://img.shields.io/docker/pulls/caesiumcloud/caesium?style=plastic
