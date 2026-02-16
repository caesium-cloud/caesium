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

  connect(filters?: Record<string, string>) {
    if (this.eventSource) {
        // If already connected with same filters, do nothing?
        // Simple check: strict equality of url params? 
        // For now, close and reconnect to be safe and simple.
      this.eventSource.close();
    }

    const params = new URLSearchParams(filters);
    const url = `/v1/events?${params.toString()}`;

    this.eventSource = new EventSource(url);

    this.eventSource.onmessage = (message) => {
      try {
        const event = JSON.parse(message.data) as CaesiumEvent;
        this.emit(event);
      } catch (err) {
        console.error("Failed to parse SSE message", err);
      }
    };

    this.eventSource.onerror = (err) => {
      console.error("SSE Error", err);
      this.eventSource?.close();
      this.eventSource = null;
      // Reconnect logic
      if (!this.reconnectTimer) {
        this.reconnectTimer = setTimeout(() => {
          this.reconnectTimer = null;
          this.connect(filters);
        }, 5000);
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
