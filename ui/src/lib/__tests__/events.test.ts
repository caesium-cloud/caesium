import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

// Mock EventSource before importing events module
class MockEventSource {
  url: string;
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  close = vi.fn();
  private eventListeners: Record<string, ((event: MessageEvent) => void)[]> = {};

  constructor(url: string) {
    this.url = url;
    MockEventSource.instances.push(this);
  }

  addEventListener(type: string, listener: (event: MessageEvent) => void) {
    if (!this.eventListeners[type]) {
      this.eventListeners[type] = [];
    }
    this.eventListeners[type].push(listener);
  }

  emit(type: string, payload: unknown, lastEventId = "") {
    const event = { data: JSON.stringify(payload), lastEventId } as MessageEvent;
    this.eventListeners[type]?.forEach((listener) => listener(event));
  }

  static instances: MockEventSource[] = [];
  static clear() {
    MockEventSource.instances = [];
  }
}

vi.stubGlobal('EventSource', MockEventSource);

// Use dynamic import so the mock is in place
let events: typeof import('@/lib/events').events;

describe('EventManager', () => {
  beforeEach(async () => {
    MockEventSource.clear();
    vi.useFakeTimers();
    // Re-import fresh module each test
    vi.resetModules();
    const mod = await import('@/lib/events');
    events = mod.events;
  });

  afterEach(() => {
    events.disconnect();
    vi.useRealTimers();
  });

  it('connect creates EventSource with correct URL', () => {
    events.connect({ job_id: 'abc' });
    expect(MockEventSource.instances.length).toBe(1);
    expect(MockEventSource.instances[0].url).toBe('/v1/events?job_id=abc');
  });

  it('subscribe registers handler that receives events', () => {
    events.connect();
    const handler = vi.fn();
    events.subscribe('run_started', handler);

    // Simulate the named SSE events emitted by the server.
    const es = MockEventSource.instances[0];
    es.emit('run_started', { type: 'run_started', timestamp: '2024-01-01T00:00:00Z' });

    expect(handler).toHaveBeenCalledWith(
      expect.objectContaining({ type: 'run_started' })
    );
  });

  it('dispatches the backend run_retried event', () => {
    events.connect();
    const handler = vi.fn();
    events.subscribe('run_retried', handler);
    MockEventSource.instances[0].emit('run_retried', { run_id: 'run-1', payload: { id: 'run-1', status: 'running' } });
    expect(handler).toHaveBeenCalledWith(expect.objectContaining({ type: 'run_retried', run_id: 'run-1' }));
  });

  it('unsubscribe removes handler', () => {
    events.connect();
    const handler = vi.fn();
    events.subscribe('run_started', handler);
    events.unsubscribe('run_started', handler);

    const es = MockEventSource.instances[0];
    es.emit('run_started', { type: 'run_started', timestamp: '2024-01-01T00:00:00Z' });

    expect(handler).not.toHaveBeenCalled();
  });

  it('a stream that never opens is unhealthy so polling can resume', () => {
    events.connect();
    const es = MockEventSource.instances[0];
    es.onerror?.();
    expect(events.isHealthy()).toBe(false);
  });

  it('repeated errors after an open stop counting as healthy', () => {
    events.connect();
    const first = MockEventSource.instances[0];
    first.onopen?.();
    expect(events.isHealthy()).toBe(true);
    first.onerror?.();
    expect(events.isHealthy()).toBe(true);
    vi.advanceTimersByTime(3000);
    MockEventSource.instances.at(-1)?.onerror?.();
    vi.advanceTimersByTime(3000);
    MockEventSource.instances.at(-1)?.onerror?.();
    expect(events.isHealthy()).toBe(false);
  });

  it('resumes a manual reconnect from the last delivered event ID', () => {
    events.connect({ job_id: 'abc' });
    const source = MockEventSource.instances[0];
    source.emit('run_started', { type: 'run_started' }, '41');
    source.emit('task_started', { type: 'task_started' }); // ID-less events cannot erase the cursor.
    source.onerror?.();
    vi.advanceTimersByTime(3000);
    expect(MockEventSource.instances.at(-1)?.url).toBe('/v1/events?job_id=abc&cursor=41');
  });

  it('retains the cursor for equivalent filters and resets it for a new scope', () => {
    events.connect({ job_id: 'abc', types: 'run_started' });
    MockEventSource.instances[0].emit('run_started', {}, '41');
    events.connect({ types: 'run_started', job_id: 'abc' });
    expect(MockEventSource.instances.at(-1)?.url).toContain('cursor=41');
    events.connect({ job_id: 'different' });
    expect(MockEventSource.instances.at(-1)?.url).toBe('/v1/events?job_id=different');
  });

  it('cancels a pending reconnect and ignores late callbacks from the replaced source', () => {
    events.connect({ job_id: 'abc' });
    const old = MockEventSource.instances[0];
    old.onerror?.();
    events.connect({ job_id: 'different' });
    const current = MockEventSource.instances.at(-1)!;
    old.emit('run_started', {}, '41');
    old.onerror?.();
    vi.advanceTimersByTime(3000);
    expect(MockEventSource.instances).toHaveLength(2);
    expect(current.close).not.toHaveBeenCalled();
    current.onerror?.();
    vi.advanceTimersByTime(3000);
    expect(MockEventSource.instances.at(-1)?.url).toBe('/v1/events?job_id=different');
  });

  it('starts a new session without a cursor after disconnect', () => {
    events.connect();
    MockEventSource.instances[0].emit('run_started', {}, '41');
    events.disconnect();
    events.connect();
    expect(MockEventSource.instances.at(-1)?.url).toBe('/v1/events?');
  });

  it('disconnect closes EventSource', () => {
    events.connect();
    const es = MockEventSource.instances[0];
    events.disconnect();
    expect(es.close).toHaveBeenCalled();
  });
});
