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
});

function modelEnum(path: string, enumType: string): string[] {
  const source = readFileSync(new URL(`../../../../${path}`, import.meta.url), "utf8");
  return [...source.matchAll(new RegExp(`\\b${enumType}\\w*\\s+${enumType}\\s*=\\s*"([^"]+)"`, "g"))]
    .map((match) => match[1]);
}
