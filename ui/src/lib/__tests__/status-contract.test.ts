// eslint-disable-next-line @typescript-eslint/ban-ts-comment
// @ts-nocheck -- this Node-only test is excluded from the browser bundle.
/** @vitest-environment node */

import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import {
  AGENT_ACTION_STATUSES,
  AGENT_SESSION_STATES,
  INCIDENT_STATUSES,
} from "../api";

describe("status registry contract", () => {
  it("covers every backend incident, action, and session enum", () => {
    expect(INCIDENT_STATUSES).toEqual(modelEnum("internal/models/incident.go", "IncidentStatus"));
    expect(AGENT_ACTION_STATUSES).toEqual(modelEnum("internal/models/agent_action.go", "AgentActionStatus"));
    expect(AGENT_SESSION_STATES).toEqual(modelEnum("internal/models/agent_session.go", "AgentSessionState"));
  });

  it("rejects a named enum constant that omits its type", () => {
    expect(() => modelEnumFromSource(`
      const (
        IncidentStatusOpen IncidentStatus = "open"
      )
      const IncidentStatusDeferred = "deferred"
    `, "IncidentStatus")).toThrow(/missing IncidentStatus type/);
  });

  it("includes later enum const blocks and rejects unsupported expressions", () => {
    expect(modelEnumFromSource(`
      const (
        IncidentStatusOpen IncidentStatus = "open"
      )
      const IncidentStatusDeferred IncidentStatus = "deferred"
    `, "IncidentStatus")).toEqual(["open", "deferred"]);

    expect(() => modelEnumFromSource(`
      const (
        IncidentStatusOpen IncidentStatus = nextStatus()
      )
    `, "IncidentStatus")).toThrow(/unsupported IncidentStatus declaration/);
  });
});

function modelEnum(path: string, enumType: string): string[] {
  return modelEnumFromSource(readFileSync(new URL(`../../../../${path}`, import.meta.url), "utf8"), enumType);
}

function modelEnumFromSource(source: string, enumType: string): string[] {
  const declarations = extractEnumDeclarations(source, enumType);
  expect(declarations).not.toHaveLength(0);
  expect(declarations.every((declaration) => declaration.type === enumType), `missing ${enumType} type`).toBe(true);
  return declarations.map((declaration) => declaration.value);
}

function extractEnumDeclarations(source: string, enumType: string) {
  const declarations: Array<{ name: string; type?: string; value: string }> = [];
  for (const block of source.matchAll(/const\s*\(([\s\S]*?)\n\s*\)/g)) {
    for (const line of (block[1] ?? "").split("\n")) {
      const declaration = parseEnumDeclaration(line, enumType);
      if (declaration) declarations.push(declaration);
    }
  }
  for (const standalone of source.matchAll(new RegExp(`^\\s*const\\s+(${enumType}\\w*\\b.*)$`, "gm"))) {
    const declaration = parseEnumDeclaration(standalone[1]!, enumType);
    if (declaration) declarations.push(declaration);
  }
  return declarations;
}

function parseEnumDeclaration(line: string, enumType: string) {
  if (!new RegExp(`^\\s*${enumType}\\w*\\b`).test(line)) return null;

  const declaration = line.match(new RegExp(`^\\s*(${enumType}\\w*)\\s*(?:(${enumType})\\s*)?=\\s*"([^"]+)"\\s*(?://.*)?$`));
  if (!declaration) {
    throw new Error(`unsupported ${enumType} declaration: ${line.trim()}`);
  }
  return { name: declaration[1]!, type: declaration[2], value: declaration[3]! };
}
