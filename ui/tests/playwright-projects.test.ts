// @vitest-environment node

import { execFileSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import path from "node:path";

interface PlaywrightListReport {
  errors: unknown[];
  suites: PlaywrightSuite[];
}

interface PlaywrightSuite {
  file?: string;
  suites?: PlaywrightSuite[];
  specs?: Array<{
    file: string;
    line: number;
    title: string;
    tests: Array<{ projectName: string }>;
  }>;
}

type ProjectMembership = Map<string, Set<string>>;

const uiRoot = process.cwd();
const playwrightCLI = path.join(uiRoot, "node_modules/playwright/cli.js");

function listedProjects(project?: string): ProjectMembership {
  const args = [playwrightCLI, "test", "--list", "--reporter=json"];
  if (project) args.push(`--project=${project}`);
  const report = JSON.parse(
    execFileSync(process.execPath, args, {
      cwd: uiRoot,
      encoding: "utf8",
      timeout: 30_000,
    }),
  ) as PlaywrightListReport;
  expect(report.errors).toEqual([]);

  const membership: ProjectMembership = new Map();
  const visit = (suite: PlaywrightSuite) => {
    for (const spec of suite.specs ?? []) {
      const identity = `${spec.file}:${spec.line}:${spec.title}`;
      for (const listed of spec.tests) {
        const tests = membership.get(listed.projectName) ?? new Set<string>();
        tests.add(identity);
        membership.set(listed.projectName, tests);
      }
    }
    for (const child of suite.suites ?? []) visit(child);
  };
  for (const suite of report.suites) visit(suite);
  return membership;
}

function union(...sets: Set<string>[]): Set<string> {
  return new Set(sets.flatMap((set) => [...set]));
}

function intersection(left: Set<string>, right: Set<string>): Set<string> {
  return new Set([...left].filter((item) => right.has(item)));
}

test("network recovery selection retains every ordinary browser scenario", () => {
  const browserReport = path.join(uiRoot, "playwright-results.json");
  const reportBefore = existsSync(browserReport) ? readFileSync(browserReport) : undefined;
  const all = listedProjects();
  expect([...all.keys()].sort()).toEqual(["auth", "default", "network-recovery"]);

  const ordinary = all.get("default") ?? new Set<string>();
  const recovery = all.get("network-recovery") ?? new Set<string>();
  const auth = all.get("auth") ?? new Set<string>();
  for (const membership of [ordinary, recovery, auth]) expect(membership.size).toBeGreaterThan(0);

  expect(intersection(ordinary, recovery)).toEqual(new Set());
  expect(intersection(ordinary, auth)).toEqual(new Set());
  expect(intersection(recovery, auth)).toEqual(new Set());
  expect([...auth].every((identity) => identity.startsWith("auth/"))).toBe(true);
  expect([...union(ordinary, recovery)].some((identity) => identity.startsWith("auth/"))).toBe(false);
  expect([...recovery].every((identity) => identity.startsWith("network-recovery.spec.ts:"))).toBe(true);

  const selected = listedProjects("network-recovery");
  expect([...selected.keys()].sort()).toEqual(["default", "network-recovery"]);
  expect(selected.get("default")).toEqual(ordinary);
  expect(selected.get("network-recovery")).toEqual(recovery);
  expect(union(...selected.values())).toEqual(union(ordinary, recovery));
  const reportAfter = existsSync(browserReport) ? readFileSync(browserReport) : undefined;
  expect(reportAfter).toEqual(reportBefore);
}, 60_000);
