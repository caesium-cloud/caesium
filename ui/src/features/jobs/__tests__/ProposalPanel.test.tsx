import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { ProposalPanel } from "../ProposalPanel";

const validArtifactRef = JSON.stringify({
  caesiumOutputRef: 1,
  path: "/workspace/tf.plan",
  digest: "sha256:" + "a".repeat(64),
  size: 2048,
});

describe("ProposalPanel", () => {
  it("renders nothing when the output carries no proposal_kind", () => {
    const { container } = render(<ProposalPanel output={{ some_output: "value" }} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing when there is no output at all", () => {
    const { container } = render(<ProposalPanel />);
    expect(container).toBeEmptyDOMElement();
  });

  it("renders the terraform.plan.v1 counts, resource table, and artifact reference", () => {
    const summary = {
      add: 2,
      change: 1,
      destroy: 0,
      resources: [
        { address: "aws_instance.web", action: "add" },
        { address: "aws_instance.api", action: "add" },
        { address: "aws_security_group.web", action: "change" },
      ],
    };

    render(
      <ProposalPanel
        output={{
          proposal_kind: "terraform.plan.v1",
          proposal_summary: JSON.stringify(summary),
          proposal_artifact: "plan",
          plan: validArtifactRef,
        }}
      />,
    );

    expect(screen.getByTestId("proposal-kind")).toHaveTextContent("terraform.plan.v1");

    const counts = screen.getAllByTestId("proposal-count");
    expect(counts).toHaveLength(3);
    expect(counts.find((el) => el.dataset.action === "add")).toHaveTextContent("2");
    expect(counts.find((el) => el.dataset.action === "change")).toHaveTextContent("1");
    expect(counts.find((el) => el.dataset.action === "destroy")).toHaveTextContent("0");

    const rows = screen.getAllByTestId("proposal-resource-row");
    expect(rows).toHaveLength(3);
    expect(rows[0]).toHaveAttribute("data-address", "aws_instance.web");
    expect(rows[0]).toHaveAttribute("data-action", "add");

    expect(screen.queryByTestId("proposal-resources-truncated")).not.toBeInTheDocument();

    const artifact = screen.getByTestId("proposal-artifact");
    expect(artifact).toHaveTextContent("plan");
    expect(screen.getByTestId("proposal-artifact-digest")).toHaveTextContent(`sha256:${"a".repeat(64)}`);
    expect(screen.getByTestId("proposal-artifact-size")).toHaveTextContent("2.0 KB");
    expect(screen.getByTestId("proposal-artifact-path")).toHaveTextContent("/workspace/tf.plan");

    // Never a download affordance — the Console renders the reference, not the data.
    expect(artifact.querySelector("a")).not.toBeInTheDocument();
    expect(artifact.querySelector("button")).not.toBeInTheDocument();
  });

  it("caps the terraform.plan.v1 resource table and notes the truncation", () => {
    const resources = Array.from({ length: 205 }, (_, i) => ({
      address: `aws_instance.host_${i}`,
      action: "add",
    }));

    render(
      <ProposalPanel
        output={{
          proposal_kind: "terraform.plan.v1",
          proposal_summary: JSON.stringify({ add: 205, change: 0, destroy: 0, resources }),
        }}
      />,
    );

    expect(screen.getAllByTestId("proposal-resource-row")).toHaveLength(200);
    expect(screen.getByTestId("proposal-resources-truncated")).toHaveTextContent("first 200");
  });

  it("skips malformed resource entries without crashing", () => {
    render(
      <ProposalPanel
        output={{
          proposal_kind: "terraform.plan.v1",
          proposal_summary: JSON.stringify({
            add: 1,
            resources: [
              { address: "aws_instance.web", action: "add" },
              { address: "missing-action" },
              "not-an-object",
              null,
              42,
            ],
          }),
        }}
      />,
    );

    const rows = screen.getAllByTestId("proposal-resource-row");
    expect(rows).toHaveLength(1);
    expect(rows[0]).toHaveAttribute("data-address", "aws_instance.web");
  });

  it("falls back to generic key/value rendering for an unknown proposal_kind", () => {
    render(
      <ProposalPanel
        output={{
          proposal_kind: "dbt.compile.v1",
          proposal_summary: JSON.stringify({ models_compiled: 12, warnings: 0 }),
        }}
      />,
    );

    expect(screen.getByTestId("proposal-kind")).toHaveTextContent("dbt.compile.v1");
    expect(screen.queryByTestId("proposal-counts")).not.toBeInTheDocument();
    expect(screen.queryByTestId("proposal-resource-table")).not.toBeInTheDocument();

    const entries = screen.getAllByTestId("proposal-generic-entry");
    expect(entries).toHaveLength(2);
    expect(entries.find((el) => el.dataset.key === "models_compiled")).toHaveTextContent("12");
    expect(entries.find((el) => el.dataset.key === "warnings")).toHaveTextContent("0");
    expect(screen.queryByTestId("proposal-summary-parse-error")).not.toBeInTheDocument();
  });

  it("falls back to raw text when proposal_summary is not valid JSON", () => {
    render(
      <ProposalPanel
        output={{
          proposal_kind: "terraform.plan.v1",
          proposal_summary: "not json at all",
        }}
      />,
    );

    expect(screen.getByTestId("proposal-summary-parse-error")).toBeInTheDocument();
    const entries = screen.getAllByTestId("proposal-generic-entry");
    expect(entries).toHaveLength(1);
    expect(entries[0]).toHaveTextContent("not json at all");
  });

  it("falls back to raw text when proposal_summary is missing entirely", () => {
    render(<ProposalPanel output={{ proposal_kind: "terraform.plan.v1" }} />);

    const entries = screen.getAllByTestId("proposal-generic-entry");
    expect(entries).toHaveLength(1);
    expect(entries[0]).toHaveTextContent("(empty)");
  });

  it("falls back to generic rendering when terraform.plan.v1's summary has no recognizable counts or resources", () => {
    render(
      <ProposalPanel
        output={{
          proposal_kind: "terraform.plan.v1",
          proposal_summary: JSON.stringify({ note: "unexpected shape" }),
        }}
      />,
    );

    expect(screen.queryByTestId("proposal-counts")).not.toBeInTheDocument();
    const entries = screen.getAllByTestId("proposal-generic-entry");
    expect(entries.find((el) => el.dataset.key === "note")).toHaveTextContent("unexpected shape");
  });

  it("reports a malformed artifact reference without crashing", () => {
    render(
      <ProposalPanel
        output={{
          proposal_kind: "terraform.plan.v1",
          proposal_summary: JSON.stringify({ add: 1, change: 0, destroy: 0, resources: [] }),
          proposal_artifact: "plan",
          plan: "not a valid output-ref",
        }}
      />,
    );

    expect(screen.getByTestId("proposal-artifact-missing")).toHaveTextContent("plan");
    expect(screen.queryByTestId("proposal-artifact")).not.toBeInTheDocument();
  });

  it("reports a missing artifact reference when proposal_artifact names an absent key", () => {
    render(
      <ProposalPanel
        output={{
          proposal_kind: "terraform.plan.v1",
          proposal_summary: JSON.stringify({ add: 0, change: 0, destroy: 0 }),
          proposal_artifact: "plan",
        }}
      />,
    );

    expect(screen.getByTestId("proposal-artifact-missing")).toBeInTheDocument();
  });

  it("renders zero-count terraform.plan.v1 summaries without an artifact (the no-changes case)", () => {
    render(
      <ProposalPanel
        output={{
          proposal_kind: "terraform.plan.v1",
          proposal_summary: JSON.stringify({ add: 0, change: 0, destroy: 0, resources: [] }),
        }}
      />,
    );

    const counts = screen.getAllByTestId("proposal-count");
    expect(counts.map((el) => el.dataset.action)).toEqual(["add", "change", "destroy"]);
    expect(counts.every((el) => el.textContent?.includes("0"))).toBe(true);
    expect(screen.queryByTestId("proposal-resource-table")).not.toBeInTheDocument();
    expect(screen.queryByTestId("proposal-artifact")).not.toBeInTheDocument();
    expect(screen.queryByTestId("proposal-artifact-missing")).not.toBeInTheDocument();
  });

  it("never renders `outputs` or `resources_truncated` as action badges against the real tf-plan summary shape", () => {
    // The literal JSON `pack/internal/tf.Summary.Encode()` produces for a
    // populated plan — same key set and field order as the struct tags in
    // pack/internal/tf/summary.go: `changes`, `add`, `change`, `destroy`,
    // `replace`, `import`, `outputs`, `resources`, `resources_truncated`.
    // `outputs` has no `omitempty`, so it is present on every real proposal;
    // `resources_truncated` is present whenever a plan exceeds
    // MaxProposalResources. Neither is an action, and before the explicit
    // skip list both rendered as bogus badges ("Outputs 0", "Resources_
    // truncated 3") because the fallback loop treated "numeric" as "action".
    const realSummaryFixture = JSON.stringify({
      changes: true,
      add: 2,
      change: 1,
      destroy: 0,
      replace: 0,
      import: 0,
      outputs: 4,
      resources: [
        { address: "aws_instance.web", action: "add" },
        { address: "aws_instance.api", action: "add" },
        { address: "aws_security_group.web", action: "change" },
      ],
      resources_truncated: 3,
    });

    render(
      <ProposalPanel
        output={{
          proposal_kind: "terraform.plan.v1",
          proposal_summary: realSummaryFixture,
        }}
      />,
    );

    const counts = screen.getAllByTestId("proposal-count");
    // Exactly the five known action buckets — never `changes`, `outputs`, or
    // `resources_truncated`.
    expect(counts.map((el) => el.dataset.action).sort()).toEqual(
      ["add", "change", "destroy", "import", "replace"].sort(),
    );
    expect(counts.find((el) => el.dataset.action === "outputs")).toBeUndefined();
    expect(counts.find((el) => el.dataset.action === "resources_truncated")).toBeUndefined();
    expect(counts.find((el) => el.dataset.action === "changes")).toBeUndefined();

    expect(screen.getAllByTestId("proposal-resource-row")).toHaveLength(3);
  });
});
