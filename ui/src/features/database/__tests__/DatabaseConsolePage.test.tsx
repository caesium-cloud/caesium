import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { DatabaseConsolePage } from "../DatabaseConsolePage";
import type { DatabaseQueryResponse, DatabaseSchemaResponse } from "@/lib/api";

vi.mock("@/lib/api", () => {
  const mockApi = {
    getDatabaseSchema: vi.fn(),
    queryDatabase: vi.fn(),
  };
  return {
    api: mockApi,
    ApiError: class extends Error {
      status: number;
      constructor(status: number, message: string) {
        super(message);
        this.status = status;
      }
    },
  };
});

import { api } from "@/lib/api";

const schema: DatabaseSchemaResponse = {
  dialect: "dqlite",
  version: "3.45.1",
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
  ],
};

const queryResult: DatabaseQueryResponse = {
  dialect: "dqlite",
  read_only: true,
  statement_type: "select",
  query: "SELECT * FROM jobs LIMIT 50;",
  limit: 200,
  duration_ms: 4,
  row_count: 1,
  truncated: false,
  columns: [
    { name: "id", data_type: "uuid" },
    { name: "alias", data_type: "text" },
  ],
  rows: [["job-1", "alpha"]],
};

function createWrapper() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return ({ children }: { children: React.ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  );
}

describe("DatabaseConsolePage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    window.localStorage.clear();
  });

  it("renders schema data and executes a query", async () => {
    vi.mocked(api.getDatabaseSchema).mockResolvedValue(schema);
    vi.mocked(api.queryDatabase).mockResolvedValue(queryResult);

    render(<DatabaseConsolePage />, { wrapper: createWrapper() });

    await waitFor(() => {
      expect(screen.getByText("Schema Explorer")).toBeInTheDocument();
    });

    await waitFor(() => {
      expect(screen.getByText("2 rows")).toBeInTheDocument();
    });

    const editor = screen.getByLabelText("SQL query editor");
    fireEvent.change(editor, { target: { value: "SELECT * FROM jobs LIMIT 50;", selectionStart: 28 } });
    fireEvent.click(screen.getByRole("button", { name: "Run query" }));

    await waitFor(() => {
      expect(screen.getByText("alpha")).toBeInTheDocument();
    });

    expect(vi.mocked(api.queryDatabase).mock.calls[0]?.[0]).toEqual({
      sql: "SELECT * FROM jobs LIMIT 50;",
      limit: 200,
    });
  });
});
