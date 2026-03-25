import { describe, it, expect, vi, beforeEach } from 'vitest';
import { api, ApiError } from '@/lib/api';

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

/** Builds a minimal Response-like mock that satisfies the request() helper. */
function okResponse(body: unknown) {
  return {
    ok: true,
    status: 200,
    headers: { get: (name: string) => (name === 'content-type' ? 'application/json' : null) },
    text: () => Promise.resolve(JSON.stringify(body)),
  };
}

beforeEach(() => {
  vi.clearAllMocks();
});

describe('api', () => {
  it('getJobs calls fetch with correct URL', async () => {
    mockFetch.mockResolvedValue(okResponse([]));
    await api.getJobs();
    expect(mockFetch).toHaveBeenCalledWith(
      '/v1/jobs',
      expect.objectContaining({
        headers: expect.objectContaining({ 'Content-Type': 'application/json' }),
      })
    );
  });

  it('getJob includes ID in URL', async () => {
    mockFetch.mockResolvedValue(okResponse({}));
    await api.getJob('test-id-123');
    expect(mockFetch).toHaveBeenCalledWith(
      '/v1/jobs/test-id-123',
      expect.anything()
    );
  });

  it('non-ok response throws ApiError with status code', async () => {
    mockFetch.mockResolvedValue({
      ok: false,
      status: 404,
      text: () => Promise.resolve('Not found'),
    });
    try {
      await api.getJobs();
      expect.unreachable('Should have thrown');
    } catch (err) {
      expect(err).toBeInstanceOf(ApiError);
      expect((err as ApiError).status).toBe(404);
    }
  });

  it('getStats returns parsed JSON', async () => {
    const statsData = {
      jobs: { total: 10, recent_runs: 5, success_rate: 0.9, avg_duration_seconds: 5.0 },
      top_failing: [],
      slowest_jobs: [],
      success_rate_trend: [{ date: '2026-03-24', run_count: 3, success_rate: 1 }],
    };
    mockFetch.mockResolvedValue(okResponse(statsData));
    const result = await api.getStats();
    expect(result).toEqual(statsData);
  });
});
