# Caesium Documentation

This directory contains user-facing guides, schema references, and planning notes.

## Start Here

- [job-definitions.md](job-definitions.md): Authoring, linting, diffing, and applying job manifests.
- [airflow-parity.md](airflow-parity.md): Progress notes for Airflow-compatible job semantics and operator controls.
- [ui_implementation_plan.md](ui_implementation_plan.md): Embedded web UI architecture and delivery plan.
- [kubernetes-deployment.md](kubernetes-deployment.md): Deploying Caesium to Kubernetes with Helm.
- [parallel-execution-operations.md](parallel-execution-operations.md): Configuration, rollout, and troubleshooting for local/distributed parallel execution.

## Reference

- [job-schema-reference.md](job-schema-reference.md): Generated schema reference from `pkg/jobdef`.
- [examples/](examples/): Example manifest files used by docs and conformance tests.

## Planning

- [job-definition-plan.md](job-definition-plan.md): Implementation roadmap and checklist for job definition ingestion and DAG execution.
- [ui_implementation_plan.md](ui_implementation_plan.md): React/Vite roadmap for the embedded operator UI.

## Historical Notes

- [architecture-history.md](architecture-history.md): Consolidated early design notes (primitives, dependency strategy, and scheduler landscape research).
