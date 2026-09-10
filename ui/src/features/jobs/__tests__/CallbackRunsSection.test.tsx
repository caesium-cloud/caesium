import { fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { CallbackRun } from "@/lib/api";
import { CallbackRunsSection } from "../CallbackRunsSection";

function makeCallback(overrides: Partial<CallbackRun> = {}): CallbackRun {
  return {
    id: "11111111-1111-4111-8111-111111111111",
    callback_id: "22222222-2222-4222-8222-222222222222",
    status: "succeeded",
    started_at: "2026-09-01T00:00:00.000Z",
    completed_at: "2026-09-01T00:00:01.000Z",
    ...overrides,
  };
}

describe("CallbackRunsSection", () => {
  it("renders the HTTP status and the retry count of a failed delivery", () => {
    render(
      <CallbackRunsSection
        callbacks={[
          makeCallback({
            status: "failed",
            error: "webhook responded 500: receiver is down",
            http_status: 500,
            retry_count: 2,
          }),
        ]}
      />,
    );

    const row = screen.getByTestId("run-callback-row");
    expect(within(row).getByTestId("run-callback-http-status")).toHaveTextContent("HTTP 500");
    expect(within(row).getByTestId("run-callback-retry-count")).toHaveTextContent("2 retries");
    expect(within(row).getByTestId("run-callback-error")).toHaveTextContent(
      "webhook responded 500: receiver is down",
    );
  });

  it("singularises a single retry", () => {
    render(<CallbackRunsSection callbacks={[makeCallback({ status: "failed", retry_count: 1 })]} />);
    expect(screen.getByTestId("run-callback-retry-count")).toHaveTextContent("1 retry");
  });

  it("keeps the response body collapsed until it is expanded", () => {
    render(
      <CallbackRunsSection
        callbacks={[
          makeCallback({ status: "failed", http_status: 503, response_body: "receiver is draining" }),
        ]}
      />,
    );

    expect(screen.queryByTestId("run-callback-response-body")).not.toBeInTheDocument();

    const toggle = screen.getByTestId("run-callback-body-toggle");
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(toggle);

    expect(screen.getByTestId("run-callback-response-body")).toHaveTextContent("receiver is draining");
    expect(screen.getByTestId("run-callback-body-toggle")).toHaveAttribute("aria-expanded", "true");

    fireEvent.click(screen.getByTestId("run-callback-body-toggle"));
    expect(screen.queryByTestId("run-callback-response-body")).not.toBeInTheDocument();
  });

  it("omits the new fields entirely for a callback that has none", () => {
    // A run recorded before the enrichment landed, and a successful transport
    // failure that never got a status back, must not render empty chrome.
    render(<CallbackRunsSection callbacks={[makeCallback()]} />);

    expect(screen.queryByTestId("run-callback-http-status")).not.toBeInTheDocument();
    expect(screen.queryByTestId("run-callback-retry-count")).not.toBeInTheDocument();
    expect(screen.queryByTestId("run-callback-body-toggle")).not.toBeInTheDocument();
  });

  it("gives each row its own independently toggled body", () => {
    render(
      <CallbackRunsSection
        callbacks={[
          makeCallback({ id: "run-a", status: "failed", response_body: "first body" }),
          makeCallback({ id: "run-b", status: "failed", response_body: "second body" }),
        ]}
      />,
    );

    const toggles = screen.getAllByTestId("run-callback-body-toggle");
    expect(toggles).toHaveLength(2);
    fireEvent.click(toggles[0]);

    const bodies = screen.getAllByTestId("run-callback-response-body");
    expect(bodies).toHaveLength(1);
    expect(bodies[0]).toHaveTextContent("first body");
  });
});
