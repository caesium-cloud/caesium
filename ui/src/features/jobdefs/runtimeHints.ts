import { parseAllDocuments } from "yaml";
import { isRecord } from "@/lib/typeGuards";

export interface JobDefRuntimeHints {
  volumeCount: number;
  volumeMountCount: number;
  volumeMountedStepCount: number;
  serviceAccountNames: string[];
  hasPodAnnotations: boolean;
  hasAutomountTokenSetting: boolean;
  parseError?: string;
}

export function getJobDefRuntimeHints(yamlSource: string): JobDefRuntimeHints {
  const hints: JobDefRuntimeHints = {
    volumeCount: 0,
    volumeMountCount: 0,
    volumeMountedStepCount: 0,
    serviceAccountNames: [],
    hasPodAnnotations: false,
    hasAutomountTokenSetting: false,
  };
  const serviceAccounts = new Set<string>();

  try {
    const docs = parseAllDocuments(yamlSource);
    for (const doc of docs) {
      if (doc.errors.length > 0) {
        hints.parseError = doc.errors[0]?.message || "Invalid YAML";
        continue;
      }

      collectDocumentHints(doc.toJS(), hints, serviceAccounts);
    }
  } catch (err) {
    hints.parseError = err instanceof Error ? err.message : "Invalid YAML";
  }

  hints.serviceAccountNames = [...serviceAccounts];
  return hints;
}

function collectDocumentHints(
  value: unknown,
  hints: JobDefRuntimeHints,
  serviceAccounts: Set<string>,
) {
  if (!isRecord(value)) return;

  const volumes = value.volumes;
  if (Array.isArray(volumes)) {
    hints.volumeCount += volumes.length;
  }

  collectIdentityHints(value.metadata, hints, serviceAccounts);

  const steps = value.steps;
  if (!Array.isArray(steps)) return;

  for (const step of steps) {
    if (!isRecord(step)) continue;
    collectIdentityHints(step, hints, serviceAccounts);

    const mounts = step.volumeMounts;
    if (Array.isArray(mounts) && mounts.length > 0) {
      hints.volumeMountCount += mounts.length;
      hints.volumeMountedStepCount += 1;
    }
  }
}

function collectIdentityHints(
  value: unknown,
  hints: JobDefRuntimeHints,
  serviceAccounts: Set<string>,
) {
  if (!isRecord(value)) return;

  const rawServiceAccountName = value.serviceAccountName;
  if (typeof rawServiceAccountName === "string") {
    const serviceAccountName = rawServiceAccountName.trim();
    if (serviceAccountName) serviceAccounts.add(serviceAccountName);
  }

  const rawPodAnnotations = value.podAnnotations;
  if (isRecord(rawPodAnnotations) && Object.keys(rawPodAnnotations).length > 0) {
    hints.hasPodAnnotations = true;
  }

  const rawAutomountServiceAccountToken = value.automountServiceAccountToken;
  if (typeof rawAutomountServiceAccountToken === "boolean") {
    hints.hasAutomountTokenSetting = true;
  }
}
