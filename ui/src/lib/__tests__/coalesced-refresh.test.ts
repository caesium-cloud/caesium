import { afterEach, expect, it, vi } from "vitest";
import { coalescedRefresh } from "../coalesced-refresh";

afterEach(() => vi.useRealTimers());

it("allows a slow refresh to finish and follows it with one refresh for intervening events", async () => {
  vi.useFakeTimers();
  let finish!: () => void;
  const refresh = vi.fn(() => new Promise<void>(resolve => { finish = resolve; }));
  const scheduler = coalescedRefresh(refresh);
  scheduler.request();
  await vi.advanceTimersByTimeAsync(250);
  for (let i = 0; i < 20; i++) {
    scheduler.request();
    await vi.advanceTimersByTimeAsync(100);
  }
  expect(refresh).toHaveBeenCalledTimes(1);
  finish();
  await vi.advanceTimersByTimeAsync(250);
  expect(refresh).toHaveBeenCalledTimes(2);
  finish();
  await vi.advanceTimersByTimeAsync(500);
  expect(refresh).toHaveBeenCalledTimes(2);
  scheduler.dispose();
});

it("disposes pending and trailing work while allowing an in-flight promise to settle", async () => {
  vi.useFakeTimers();
  let finish!: () => void;
  const refresh = vi.fn(() => new Promise<void>(resolve => { finish = resolve; }));
  const scheduler = coalescedRefresh(refresh);
  scheduler.request();
  await vi.advanceTimersByTimeAsync(250);
  scheduler.request();
  scheduler.dispose();
  finish();
  await vi.advanceTimersByTimeAsync(1000);
  scheduler.request();
  await vi.advanceTimersByTimeAsync(1000);
  expect(refresh).toHaveBeenCalledTimes(1);
});
