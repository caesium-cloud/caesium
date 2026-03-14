import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

// Mock EventSource before importing events module
class MockEventSource {
  url: string;
  onmessage: ((event: MessageEvent) => void) | null = null;
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

    // Simulate an SSE event by triggering addEventListener callback
    const es = MockEventSource.instances[0];
    // The EventManager subscribes via addEventListener and onmessage
    // Use the onmessage handler to simulate a generic event
    es.onmessage?.({
      data: JSON.stringify({ type: 'run_started', timestamp: '2024-01-01T00:00:00Z' }),
    } as MessageEvent);

    expect(handler).toHaveBeenCalledWith(
      expect.objectContaining({ type: 'run_started' })
    );
  });

  it('unsubscribe removes handler', () => {
    events.connect();
    const handler = vi.fn();
    events.subscribe('run_started', handler);
    events.unsubscribe('run_started', handler);

    const es = MockEventSource.instances[0];
    es.onmessage?.({
      data: JSON.stringify({ type: 'run_started', timestamp: '2024-01-01T00:00:00Z' }),
    } as MessageEvent);

    expect(handler).not.toHaveBeenCalled();
  });

  it('disconnect closes EventSource', () => {
    events.connect();
    const es = MockEventSource.instances[0];
    events.disconnect();
    expect(es.close).toHaveBeenCalled();
  });
});
