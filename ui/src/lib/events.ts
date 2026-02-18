export interface CaesiumEvent {
  type: string;
  job_id?: string;
  run_id?: string;
  task_id?: string;
  timestamp: string;
  payload?: unknown;
}

type EventHandler = (event: CaesiumEvent) => void;

class EventManager {
  private eventSource: EventSource | null = null;
  private listeners: Map<string, EventHandler[]> = new Map();
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private filters: Record<string, string> = {};

  connect(filters: Record<string, string> = {}) {
    this.filters = filters;
    this.reconnect();
  }

  private reconnect() {
    if (this.eventSource) {
      this.eventSource.close();
    }

    const params = new URLSearchParams(this.filters);
    const url = `/v1/events?${params.toString()}`;

    this.eventSource = new EventSource(url);

    const eventTypes = [
      "job_created", "job_deleted", 
      "run_started", "run_completed", "run_failed",
      "task_started", "task_succeeded", "task_failed", "task_skipped",
      "log_chunk"
    ];

    eventTypes.forEach(type => {
      this.eventSource?.addEventListener(type, (message: MessageEvent) => {
        try {
          const event = JSON.parse(message.data) as CaesiumEvent;
          if (!event.type) event.type = type;
          this.emit(event);
        } catch (err) {
          console.error(`Failed to parse SSE message for ${type}`, err);
        }
      });
    });

    this.eventSource.onmessage = (message) => {
      try {
        const event = JSON.parse(message.data) as CaesiumEvent;
        this.emit(event);
      } catch (err) {
        console.error("Failed to parse SSE message", err);
      }
    };

    this.eventSource.onerror = () => {
      this.eventSource?.close();
      this.eventSource = null;
      if (!this.reconnectTimer) {
        this.reconnectTimer = setTimeout(() => {
          this.reconnectTimer = null;
          this.reconnect();
        }, 3000);
      }
    };
  }

  subscribe(eventType: string, handler: EventHandler) {
    if (!this.listeners.has(eventType)) {
      this.listeners.set(eventType, []);
    }
    this.listeners.get(eventType)?.push(handler);
  }

  unsubscribe(eventType: string, handler: EventHandler) {
    const handlers = this.listeners.get(eventType);
    if (handlers) {
      this.listeners.set(
        eventType,
        handlers.filter((h) => h !== handler)
      );
    }
  }

  disconnect() {
    if (this.eventSource) {
      this.eventSource.close();
      this.eventSource = null;
    }
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
  }

  private emit(event: CaesiumEvent) {
    const handlers = this.listeners.get(event.type);
    if (handlers) {
      handlers.forEach((h) => h(event));
    }
    // Also emit to wildcard or general listeners if needed
  }
}

export const events = new EventManager();
