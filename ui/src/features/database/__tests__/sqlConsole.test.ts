import { describe, expect, it } from "vitest";
import type { DatabaseSchemaResponse } from "@/lib/api";
import { applySqlSuggestion, buildSqlSuggestions } from "../sqlConsole";

const schema: DatabaseSchemaResponse = {
  dialect: "dqlite",
  read_only: true,
  tables: [
    {
      name: "jobs",
      row_count: 2,
      columns: [
        { name: "id", data_type: "uuid", nullable: false, primary_key: true },
        { name: "alias", data_type: "text", nullable: false, primary_key: false },
      ],
    },
    {
      name: "job_runs",
      row_count: 10,
      columns: [
        { name: "id", data_type: "uuid", nullable: false, primary_key: true },
        { name: "job_id", data_type: "uuid", nullable: false, primary_key: false },
        { name: "status", data_type: "text", nullable: false, primary_key: false },
      ],
    },
  ],
};

describe("sqlConsole helpers", () => {
  it("suggests matching tables and columns from schema", () => {
    const suggestions = buildSqlSuggestions(schema, "SELECT * FROM jo", "SELECT * FROM jo".length);
    expect(suggestions.some((suggestion) => suggestion.value === "jobs")).toBe(true);
    expect(suggestions.some((suggestion) => suggestion.value === "job_runs")).toBe(true);
    expect(suggestions.some((suggestion) => suggestion.value === "job_id")).toBe(true);
  });

  it("applies a suggestion into the current token", () => {
    const suggestion = buildSqlSuggestions(schema, "SELECT * FROM jo", "SELECT * FROM jo".length)
      .find((entry) => entry.value === "jobs");
    expect(suggestion).toBeDefined();
    if (!suggestion) {
      throw new Error("expected jobs suggestion");
    }
    const applied = applySqlSuggestion("SELECT * FROM jo", "SELECT * FROM jo".length, suggestion);
    expect(applied.sql).toContain("FROM jobs ");
  });
});
