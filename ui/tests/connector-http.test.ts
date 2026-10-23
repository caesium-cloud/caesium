// @vitest-environment node

import { createServer } from "node:http";
import type { Socket } from "node:net";
import { request } from "@playwright/test";
import { expect, test } from "vitest";
import { connectorProbeHeaders } from "../e2e/connectors/http";

interface ObservedRequest {
  connectionId: number;
  requestCount: number;
  method: string | undefined;
  path: string | undefined;
  authorization: string | undefined;
  connection: string | undefined;
  body: string;
}

async function tcpServer(abrupt = false) {
  const seen: ObservedRequest[] = [];
  const sockets = new Map<Socket, number>();
  let nextConnection = 0;
  const server = createServer((incoming, response) => {
    const chunks: Buffer[] = [];
    incoming.on("data", (chunk: Buffer) => chunks.push(chunk));
    incoming.on("end", () => {
      const observed: ObservedRequest = {
        connectionId: sockets.get(incoming.socket)!,
        requestCount: seen.length + 1,
        method: incoming.method,
        path: incoming.url,
        authorization: incoming.headers.authorization,
        connection: incoming.headers.connection,
        body: Buffer.concat(chunks).toString(),
      };
      seen.push(observed);
      if (abrupt) {
        incoming.socket.destroy();
        return;
      }
      response.setHeader("Content-Type", "application/json");
      response.end(JSON.stringify(observed));
    });
  });
  server.on("connection", socket => sockets.set(socket, ++nextConnection));
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  const address = server.address();
  if (!address || typeof address === "string") throw new Error("TCP server has no loopback port");
  return {
    url: `http://127.0.0.1:${address.port}`,
    seen,
    close: () => new Promise<void>((resolve, reject) => {
      server.close(error => error ? reject(error) : resolve());
      server.closeAllConnections();
    }),
  };
}

test("baseline Playwright GET and POST reuse one real TCP connection", async () => {
  const server = await tcpServer();
  const context = await request.newContext({ baseURL: server.url });
  try {
    const headers = { Authorization: "Bearer baseline-key" };
    const first = await context.get("/read", { headers });
    expect(first.status()).toBe(200);
    const read = await first.json() as ObservedRequest;
    const second = await context.post("/write", { headers, data: { value: "once" } });
    expect(second.status()).toBe(200);
    const write = await second.json() as ObservedRequest;
    expect(read.connectionId).toBeGreaterThan(0);
    expect(write.connectionId).toBe(read.connectionId);
    expect(server.seen).toEqual([read, write]);
    expect(server.seen.map(entry => entry.method)).toEqual(["GET", "POST"]);
    expect(server.seen.map(entry => entry.authorization)).toEqual(["Bearer baseline-key", "Bearer baseline-key"]);
    expect(JSON.parse(write.body)).toEqual({ value: "once" });
  } finally {
    await context.dispose();
    await server.close();
  }
});

test("probe headers use distinct TCP connections for GET and POST from the first request", async () => {
  const server = await tcpServer();
  const context = await request.newContext({ baseURL: server.url });
  try {
    const headers = connectorProbeHeaders({ Authorization: "Bearer probe-key", "Idempotency-Key": "single-send" });
    const first = await context.get("/read", { headers });
    expect(first.status()).toBe(200);
    const read = await first.json() as ObservedRequest;
    const second = await context.post("/write", { headers, data: { value: "once" } });
    expect(second.status()).toBe(200);
    const write = await second.json() as ObservedRequest;
    expect(server.seen).toEqual([read, write]);
    expect(new Set(server.seen.map(entry => entry.connectionId)).size).toBe(2);
    expect(server.seen.map(entry => entry.connection)).toEqual(["close", "close"]);
    expect(server.seen.map(entry => entry.authorization)).toEqual(["Bearer probe-key", "Bearer probe-key"]);
    expect(server.seen.map(entry => entry.requestCount)).toEqual([1, 2]);
    expect(server.seen.map(entry => entry.method)).toEqual(["GET", "POST"]);
    expect(JSON.parse(write.body)).toEqual({ value: "once" });
  } finally {
    await context.dispose();
    await server.close();
  }
});

test("an abrupt server close fails a probe after exactly one observed request", async () => {
  const server = await tcpServer(true);
  const context = await request.newContext({ baseURL: server.url });
  try {
    await expect(context.get("/abrupt", {
      headers: connectorProbeHeaders({ Authorization: "Bearer probe-key" }),
    })).rejects.toThrow(/ECONNRESET/);
    expect(server.seen).toHaveLength(1);
    expect(server.seen[0]).toMatchObject({ method: "GET", path: "/abrupt", authorization: "Bearer probe-key", connection: "close", requestCount: 1 });
  } finally {
    await context.dispose();
    await server.close();
  }
});
