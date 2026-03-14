import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { StatsPage } from '../StatsPage';
import type { StatsResponse } from '@/lib/api';

vi.mock('@/lib/api', () => {
  const mockApi = {
    getStats: vi.fn(),
  };
  return { api: mockApi, ApiError: class extends Error { status: number; constructor(s: number, m: string) { super(m); this.status = s; } } };
});

// Mock recharts ResponsiveContainer since jsdom has no layout
vi.mock('recharts', async () => {
  const actual = await vi.importActual<typeof import('recharts')>('recharts');
  return {
    ...actual,
    ResponsiveContainer: ({ children }: { children: React.ReactNode }) => (
      <div style={{ width: 400, height: 300 }}>{children}</div>
    ),
  };
});

import { api } from '@/lib/api';

const mockStats: StatsResponse = {
  jobs: {
    total: 42,
    recent_runs: 15,
    success_rate: 0.857,
    avg_duration_seconds: 12.5,
  },
  top_failing: [
    { job_id: 'job-1', alias: 'deploy-prod', failure_count: 5 },
    { job_id: 'job-2', alias: 'run-tests', failure_count: 3 },
  ],
  slowest_jobs: [
    { job_id: 'job-3', alias: 'build-all', avg_duration_seconds: 120.5 },
  ],
};

function createWrapper() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return ({ children }: { children: React.ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  );
}

describe('StatsPage', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('shows loading state', () => {
    vi.mocked(api.getStats).mockReturnValue(new Promise(() => {}));
    render(<StatsPage />, { wrapper: createWrapper() });
    expect(screen.getByText('Loading stats...')).toBeInTheDocument();
  });

  it('shows error state', async () => {
    vi.mocked(api.getStats).mockRejectedValue(new Error('Network error'));
    render(<StatsPage />, { wrapper: createWrapper() });
    await waitFor(() => {
      expect(screen.getByText(/Error loading stats/)).toBeInTheDocument();
    });
  });

  it('renders summary cards with correct values', async () => {
    vi.mocked(api.getStats).mockResolvedValue(mockStats);
    render(<StatsPage />, { wrapper: createWrapper() });
    await waitFor(() => {
      expect(screen.getByText('42')).toBeInTheDocument();
    });
    expect(screen.getByText('15')).toBeInTheDocument();
    expect(screen.getByText('85.7%')).toBeInTheDocument();
    expect(screen.getByText('12.50s')).toBeInTheDocument();
  });

  it('renders tables with data', async () => {
    vi.mocked(api.getStats).mockResolvedValue(mockStats);
    render(<StatsPage />, { wrapper: createWrapper() });
    await waitFor(() => {
      expect(screen.getByText('deploy-prod')).toBeInTheDocument();
    });
    expect(screen.getByText('run-tests')).toBeInTheDocument();
    expect(screen.getByText('build-all')).toBeInTheDocument();
  });

  it('shows no failures message when top_failing is empty', async () => {
    vi.mocked(api.getStats).mockResolvedValue({
      ...mockStats,
      top_failing: [],
    });
    render(<StatsPage />, { wrapper: createWrapper() });
    await waitFor(() => {
      const elements = screen.getAllByText('No failures recorded');
      expect(elements.length).toBeGreaterThanOrEqual(1);
    });
  });
});
