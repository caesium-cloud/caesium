import fs from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import type { APIRequestContext } from "@playwright/test";
import { parseAllDocuments } from "yaml";

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);

export type FixtureDefinition = {
  metadata?: Record<string, unknown> & { alias?: string };
  trigger?: {
    configuration?: Record<string, unknown> & { path?: string };
  };
};

/**
 * Load a job-definition YAML from docs/examples/, mutating the alias (and any
 * HTTP webhook path) so each test invocation gets a unique resource and tests
 * can run repeatedly without colliding with prior state.
 */
export async function loadFixtureDefinition(filename: string): Promise<FixtureDefinition> {
  const fixturePath = path.resolve(__dirname, "../../../docs/examples", filename);
  const yaml = await fs.readFile(fixturePath, "utf8");
  const docs = parseAllDocuments(yaml);
  const raw = docs[0]?.toJS();
  if (!raw || typeof raw !== "object") {
    throw new Error(`failed to parse fixture: ${filename}`);
  }
  const def = raw as FixtureDefinition;

  const suffix = `${Date.now().toString(36)}-${Math.floor(Math.random() * 1_000_000).toString(36)}`;
  const baseAlias = String(def.metadata?.alias ?? path.basename(filename, ".job.yaml"));
  def.metadata = { ...(def.metadata ?? {}), alias: `${baseAlias}-${suffix}` };

  if (def.trigger?.configuration && typeof def.trigger.configuration.path === "string") {
    def.trigger.configuration.path = `/hooks/demo/${baseAlias}-${suffix}`;
  }

  return def;
}

export async function applyDefinitions(request: APIRequestContext, ...defs: FixtureDefinition[]): Promise<void> {
  const response = await request.post("/v1/jobdefs/apply", {
    data: { definitions: defs },
  });
  if (!response.ok()) {
    throw new Error(`failed to apply fixture: ${response.status()} ${await response.text()}`);
  }
}
