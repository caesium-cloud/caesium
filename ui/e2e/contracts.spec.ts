import { expect, test, type APIRequestContext } from "@playwright/test";
import { stringify } from "yaml";
import { applyDefinitions, findJobByAlias, type FixtureDefinition } from "./helpers/fixtures";

type ContractFixtureDefinition = Omit<FixtureDefinition, "trigger"> & {
  apiVersion: string;
  kind: string;
  trigger: {
    type: string;
    configuration: Record<string, unknown>;
  };
  steps: Array<{
    name: string;
    engine: string;
    image: string;
    command: string[];
    outputSchema?: Record<string, unknown>;
  }>;
};

type ContractGraphResponse = {
  nodes: Array<{
    id: string;
    kind: string;
    alias?: string;
  }>;
  edges: ContractGraphEdge[];
};

type ContractGraphEdge = {
  id: string;
  from: string;
  to: string;
  class: string;
  verdict?: string;
};

const shellImage = "alpine:3.23";

test("operator can inspect the feature-gated contract graph", async ({ page, request }) => {
  const suffix = Date.now().toString(36);
  const producerAlias = `contract-ui-producer-${suffix}`;
  const consumerAlias = `contract-ui-consumer-${suffix}`;

  await applyDefinitions(
    request,
    buildProducerDefinition(producerAlias),
    buildConsumerDefinition(consumerAlias, producerAlias),
  );
  const producer = await findJobByAlias(request, producerAlias);
  const consumer = await findJobByAlias(request, consumerAlias);
  const edge = await waitForContractEdge(request, producerAlias, consumerAlias);

  await page.goto("/contracts");

  await expect(page.locator("aside").getByRole("link", { name: /^Contracts\b/ })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Contract graph", exact: true })).toBeVisible();
  await expect(page.getByTestId(`contract-node:job:${producerAlias}`)).toContainText(producerAlias);
  await expect(page.getByTestId(`contract-node:job:${producerAlias}`)).toHaveAttribute("href", `/jobs/${producer.id}`);
  await expect(page.getByTestId(`contract-node:job:${consumerAlias}`)).toContainText(consumerAlias);
  await expect(page.getByTestId(`contract-node:job:${consumerAlias}`)).toHaveAttribute("href", `/jobs/${consumer.id}`);

  const edgeLabel = page.getByTestId(contractEdgeTestId(edge));
  await expect(edgeLabel).toBeVisible();
  await expect(edgeLabel).toHaveAttribute("data-edge-class", "inferred");
  await expect(edgeLabel).toHaveAttribute("data-edge-verdict", "compatible");
});

test("operator can acknowledge a breaking contract finding from the JobDefs diff", async ({ page, request }) => {
  const suffix = Date.now().toString(36);
  const producerAlias = `contract-diff-producer-${suffix}`;
  const consumerAlias = `contract-diff-consumer-${suffix}`;

  await applyDefinitions(
    request,
    buildProducerDefinition(producerAlias),
    buildConsumerDefinition(consumerAlias, producerAlias),
  );
  await waitForContractEdge(request, producerAlias, consumerAlias);

  await page.goto("/jobdefs");
  await page.locator(".cm-content[contenteditable='true']").fill(stringify(buildProducerDefinition(producerAlias, [])));
  await page.getByRole("tab", { name: /Diff vs server/i }).click();

  const breakingBadge = page.getByTestId("contract-finding-badge").filter({ hasText: "breaking" }).first();
  await expect(breakingBadge).toBeVisible();
  await expect(breakingBadge).toContainText(`${producerAlias}.output.row_count`);
  await expect(breakingBadge).toContainText(`consumer: ${consumerAlias}`);

  const applyButton = page.getByRole("button", { name: /Apply definition/i });
  await expect(applyButton).toBeDisabled();

  await page.getByLabel("Breaking change acknowledgement reason").fill("accepted during contract migration");
  await expect(applyButton).toBeEnabled();
  await applyButton.click();
  await expect(page.getByText(/Applied successfully/)).toBeVisible();
});

function buildProducerDefinition(alias: string, outputKeys = ["row_count"]): ContractFixtureDefinition {
  return {
    apiVersion: "v1",
    kind: "Job",
    metadata: {
      alias,
    },
    trigger: {
      type: "cron",
      configuration: {
        cron: "0 2 * * *",
        timezone: "UTC",
      },
    },
    steps: [
      {
        name: "export",
        engine: "docker",
        image: shellImage,
        outputSchema: outputSchemaForKeys(outputKeys),
        command: ["sh", "-c", "echo export"],
      },
    ],
  };
}

function outputSchemaForKeys(keys: string[]) {
  return {
    type: "object",
    required: keys,
    properties: Object.fromEntries(keys.map((key) => [key, { type: "integer" }])),
  };
}

function buildConsumerDefinition(alias: string, producerAlias: string): ContractFixtureDefinition {
  return {
    apiVersion: "v1",
    kind: "Job",
    metadata: {
      alias,
    },
    trigger: {
      type: "event",
      configuration: {
        events: [
          {
            type: "run_completed",
            source: "caesium",
            filter: {
              job_alias: producerAlias,
            },
          },
        ],
        paramMapping: {
          upstream_rows: "$.tasks[0].output.row_count",
        },
      },
    },
    steps: [
      {
        name: "load",
        engine: "docker",
        image: shellImage,
        command: ["sh", "-c", "echo load"],
      },
    ],
  };
}

async function waitForContractEdge(
  request: APIRequestContext,
  producerAlias: string,
  consumerAlias: string,
): Promise<ContractGraphEdge> {
  let latest: ContractGraphResponse | undefined;
  const expected = `job:${producerAlias}|job:${consumerAlias}|inferred|compatible`;

  await expect
    .poll(
      async () => {
        const response = await request.get("/v1/contracts/graph");
        if (!response.ok()) {
          return "";
        }
        latest = (await response.json()) as ContractGraphResponse;
        return latest.edges
          .map((edge) => `${edge.from}|${edge.to}|${edge.class}|${edge.verdict ?? ""}`)
          .join("\n");
      },
      {
        timeout: 15_000,
        intervals: [500, 1_000, 2_000],
      },
    )
    .toContain(expected);

  const edge = latest?.edges.find(
    (candidate) =>
      candidate.from === `job:${producerAlias}` &&
      candidate.to === `job:${consumerAlias}` &&
      candidate.class === "inferred" &&
      candidate.verdict === "compatible",
  );
  if (!edge) {
    throw new Error(`contract graph did not return edge ${expected}`);
  }
  return edge;
}

function contractEdgeTestId(edge: ContractGraphEdge): string {
  return `contract-edge:${edge.class}:${edge.id}`;
}
