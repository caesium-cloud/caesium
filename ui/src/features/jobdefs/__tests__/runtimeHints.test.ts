import { describe, expect, it } from "vitest";
import { getJobDefRuntimeHints } from "../runtimeHints";

describe("getJobDefRuntimeHints", () => {
  it("summarizes volumes and Kubernetes identity fields", () => {
    const hints = getJobDefRuntimeHints(`apiVersion: v1
kind: Job
metadata:
  alias: volume-identity
  serviceAccountName: default-runner
  podAnnotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/caesium-runner
  automountServiceAccountToken: false
volumes:
  - name: work
    sources:
      kubernetes:
        pvc: ci-shared-rwx
steps:
  - name: plan
    engine: kubernetes
    image: hashicorp/terraform:1.9
    volumeMounts:
      - {volume: work, path: /work}
  - name: apply
    engine: kubernetes
    image: hashicorp/terraform:1.9
    serviceAccountName: deployer
    volumeMounts:
      - {volume: work, path: /work, readOnly: true}
`);

    expect(hints).toMatchObject({
      volumeCount: 1,
      volumeMountCount: 2,
      volumeMountedStepCount: 2,
      hasPodAnnotations: true,
      hasAutomountTokenSetting: true,
    });
    expect(hints.serviceAccountNames).toEqual(["default-runner", "deployer"]);
  });

  it("accumulates hints across multi-document manifests", () => {
    const hints = getJobDefRuntimeHints(`apiVersion: v1
kind: Job
metadata:
  alias: first
volumes:
  - name: scratch
    source:
      tmpfs: {sizeBytes: 1048576}
steps:
  - name: test
    image: alpine:3.23
    volumeMounts:
      - {volume: scratch, path: /tmp/scratch}
---
apiVersion: v1
kind: Job
metadata:
  alias: second
  serviceAccountName: batch-runner
steps:
  - name: run
    engine: kubernetes
    image: alpine:3.23
`);

    expect(hints.volumeCount).toBe(1);
    expect(hints.volumeMountCount).toBe(1);
    expect(hints.volumeMountedStepCount).toBe(1);
    expect(hints.serviceAccountNames).toEqual(["batch-runner"]);
  });

  it("reports parse errors without throwing", () => {
    const hints = getJobDefRuntimeHints("apiVersion: v1\nmetadata: [");

    expect(hints.parseError).toBeTruthy();
    expect(hints.volumeCount).toBe(0);
    expect(hints.serviceAccountNames).toEqual([]);
  });
});
