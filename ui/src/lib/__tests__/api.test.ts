import { describe, it, expect, vi, beforeEach } from 'vitest';
import { api, ApiError } from '@/lib/api';
import { clearApiKey, isAuthenticated, setApiKey } from '@/lib/auth';

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
  clearApiKey();
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

  it('getJobCache targets the cache endpoint', async () => {
    mockFetch.mockResolvedValue(okResponse({ entries: [] }));
    await api.getJobCache('job-42');
    expect(mockFetch).toHaveBeenCalledWith('/v1/jobs/job-42/cache', expect.anything());
  });

  it('getContractGraph encodes the optional dataset filter', async () => {
    mockFetch.mockResolvedValue(okResponse({ nodes: [], edges: [] }));
    await api.getContractGraph({ dataset: 'lake/customers' });
    expect(mockFetch).toHaveBeenCalledWith(
      '/v1/contracts/graph?dataset=lake%2Fcustomers',
      expect.anything(),
    );
  });

  it('getContractGraph surfaces disabled-route 404 as ApiError', async () => {
    mockFetch.mockResolvedValue({
      ok: false,
      status: 404,
      text: () => Promise.resolve('Not found'),
    });

    await expect(api.getContractGraph()).rejects.toMatchObject({ status: 404 });
  });

  it('getJobTasks normalizes task IDs from Go model casing', async () => {
    mockFetch.mockResolvedValue(okResponse([
      {
        ID: 'task-1',
        JobID: 'job-1',
        AtomID: 'atom-1',
        name: 'extract',
        node_selector: { disk: 'ssd' },
        retries: 2,
        retry_delay: 1000,
        retry_backoff: true,
        trigger_rule: 'all_success',
        cache_config: null,
        CreatedAt: '2026-06-26T00:00:00Z',
        UpdatedAt: '2026-06-26T00:00:01Z',
      },
    ]));

    await expect(api.getJobTasks('job-1')).resolves.toEqual([
      {
        id: 'task-1',
        job_id: 'job-1',
        atom_id: 'atom-1',
        name: 'extract',
        node_selector: { disk: 'ssd' },
        retries: 2,
        retry_delay: 1000,
        retry_backoff: true,
        trigger_rule: 'all_success',
        cache_config: null,
        created_at: '2026-06-26T00:00:00Z',
        updated_at: '2026-06-26T00:00:01Z',
      },
    ]);
  });

  it('applyJobDef preserves volume and workload identity fields in the request payload', async () => {
    mockFetch.mockResolvedValue(okResponse({ applied: 1 }));

    await api.applyJobDef(`apiVersion: v1
kind: Job
metadata:
  alias: runtime-contract
  serviceAccountName: caesium-reader
  podAnnotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/caesium-reader
  automountServiceAccountToken: false
trigger:
  type: http
  configuration:
    path: /hooks/runtime-contract
volumes:
  - name: workspace
    sources:
      kubernetes:
        pvc: caesium-workspace-rwx
steps:
  - name: run
    engine: kubernetes
    image: alpine:3.23
    serviceAccountName: caesium-writer
    volumeMounts:
      - {volume: workspace, path: /workspace, readOnly: true}
`);

    const [, init] = mockFetch.mock.calls[0];
    const payload = JSON.parse(String(init?.body));
    expect(payload.definitions).toHaveLength(1);
    expect(payload.definitions[0]).toMatchObject({
      metadata: {
        alias: 'runtime-contract',
        serviceAccountName: 'caesium-reader',
        podAnnotations: {
          'eks.amazonaws.com/role-arn': 'arn:aws:iam::123456789012:role/caesium-reader',
        },
        automountServiceAccountToken: false,
      },
      volumes: [
        {
          name: 'workspace',
          sources: {
            kubernetes: { pvc: 'caesium-workspace-rwx' },
          },
        },
      ],
      steps: [
        expect.objectContaining({
          name: 'run',
          engine: 'kubernetes',
          serviceAccountName: 'caesium-writer',
          volumeMounts: [
            { volume: 'workspace', path: '/workspace', readOnly: true },
          ],
        }),
      ],
    });
  });

  it('applyJobDef threads contract break acknowledgements', async () => {
    mockFetch.mockResolvedValue(okResponse({ applied: 1 }));

    await api.applyJobDef(`apiVersion: v1
kind: Job
metadata:
  alias: contract-ack
trigger:
  type: cron
  configuration:
    cron: "0 * * * *"
steps:
  - name: run
    image: alpine:3.23
`, {
      dataset: 'producer.output.customer_id',
      reason: 'accepted by reporting',
    });

    const [, init] = mockFetch.mock.calls[0];
    const payload = JSON.parse(String(init?.body));
    expect(payload.allow_breaking).toEqual({
      dataset: 'producer.output.customer_id',
      reason: 'accepted by reporting',
    });
  });

  it('deleteTaskCache encodes the task name in the URL', async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      status: 204,
      text: () => Promise.resolve(''),
    });
    await api.deleteTaskCache('job-42', 'extract data');
    expect(mockFetch).toHaveBeenCalledWith(
      '/v1/jobs/job-42/cache/extract%20data',
      expect.objectContaining({ method: 'DELETE' })
    );
  });

  it('pruneCache uses POST on the global cache prune endpoint', async () => {
    mockFetch.mockResolvedValue(okResponse({ pruned: 3 }));
    await api.pruneCache();
    expect(mockFetch).toHaveBeenCalledWith(
      '/v1/cache/prune',
      expect.objectContaining({ method: 'POST' })
    );
  });

  it('adds the bearer token header when an api key is present', async () => {
    setApiKey('csk_live_secret');
    mockFetch.mockResolvedValue(okResponse([]));

    await api.getJobs();

    expect(mockFetch).toHaveBeenCalledWith(
      '/v1/jobs',
      expect.objectContaining({
        headers: expect.objectContaining({
          Authorization: 'Bearer csk_live_secret',
        }),
      })
    );
  });

  it('clears the in-memory api key on 401 responses', async () => {
    setApiKey('csk_live_secret');
    mockFetch.mockResolvedValue({
      ok: false,
      status: 401,
      text: () => Promise.resolve('Authentication required'),
    });

    await expect(api.getJobs()).rejects.toMatchObject({ status: 401 });
    expect(isAuthenticated()).toBe(false);
  });
});
