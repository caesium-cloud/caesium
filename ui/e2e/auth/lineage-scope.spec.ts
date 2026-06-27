import { expect, test } from "@playwright/test";
import { authHeaders, obtainAuthKeys, type AuthLaneKeys } from "../helpers/auth";

let keys: AuthLaneKeys;

test.beforeAll(async ({ request }) => {
  keys = await obtainAuthKeys(request);
});

test("a job-scoped key is denied the global lineage impact query", async ({ request }) => {
  const response = await request.get("/v1/lineage/impact?namespace=caesium&name=missing", {
    headers: authHeaders(keys.scoped),
  });

  expect(response.status()).toBe(403);
  const body = await response.text();
  expect(body).toContain(
    "lineage impact is a global cross-job query and requires an unscoped principal",
  );
});
