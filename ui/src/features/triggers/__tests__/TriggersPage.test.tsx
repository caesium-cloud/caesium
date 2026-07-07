import { describe, expect, it } from "vitest";
import type { Trigger } from "@/lib/api";
import { describeTrigger, getNextFireDate } from "../trigger-utils";

const now = new Date().toISOString();

function trigger(type: string, configuration: Record<string, unknown>): Trigger {
  return {
    id: `${type}-trigger`,
    alias: `${type}-alias`,
    type,
    configuration: JSON.stringify(configuration),
    created_at: now,
    updated_at: now,
  };
}

describe("trigger cron parsing", () => {
  const expressions = ["0 * * * *", "*/15 * * * *", "0 */6 * * *", "0 2 * * *"];

  it.each(expressions)("parses %s without a timezone", (expression) => {
    const next = getNextFireDate(expression);

    expect(next).toBeInstanceOf(Date);
    expect(Number.isNaN(next?.getTime())).toBe(false);
  });

  it.each(expressions)("parses %s with a timezone", (expression) => {
    const next = getNextFireDate(expression, "UTC");

    expect(next).toBeInstanceOf(Date);
    expect(Number.isNaN(next?.getTime())).toBe(false);
  });

  it("parses the run-history fixture schedule with UTC", () => {
    const next = getNextFireDate("*/2 * * * *", "UTC");

    expect(next).toBeInstanceOf(Date);
    expect(Number.isNaN(next?.getTime())).toBe(false);
  });

  it("extracts backend-compatible schedule keys before parsing", () => {
    const description = describeTrigger(trigger("cron", {
      schedule: "0 * * * *",
      timezone: "UTC",
    }));

    expect(description).toMatchObject({
      kind: "cron",
      expression: "0 * * * *",
      timezone: "UTC",
    });

    if (description.kind !== "cron") throw new Error("expected cron trigger");
    expect(getNextFireDate(description.expression, description.timezone)).toBeInstanceOf(Date);
  });
});

describe("describeTrigger", () => {
  it("summarizes event trigger patterns without dumping JSON", () => {
    const description = describeTrigger(trigger("event", {
      events: [
        {
          type: "deployment.*",
          source: "github-actions",
          filter: {
            environment: "production",
            "repository.full_name": "caesium-cloud/caesium",
          },
        },
      ],
      paramMapping: {
        commit: "$.commit",
        actor: "$.actor",
        environment: "$.environment",
      },
      defaultParams: {
        triggered_by: "event",
        priority: "normal",
      },
    }));

    expect(description).toMatchObject({
      kind: "event",
      summary: "deployment.* from github-actions",
      detail: "2 filters, 3 mapped params, 2 default params",
    });
  });

  it("uses a concise fallback for non-cron, non-http trigger types", () => {
    const description = describeTrigger(trigger("freshness", {
      datasets: ["orders"],
      mode: "arrival",
    }));

    expect(description).toMatchObject({
      kind: "other",
      summary: "Freshness trigger",
      detail: "configuration keys: datasets, mode",
    });
  });
});
