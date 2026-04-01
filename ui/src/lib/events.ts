export interface CaesiumEvent {
  sequence?: number;
  type: string;
  job_id?: string;
  run_id?: string;
  task_id?: string;
  timestamp: string;
  payload?: unknown;
}

type EventHandler = (event: CaesiumEvent) => void;
type ConnectionHandler = (connected: boolean) => void;

class EventManager {
  private eventSource: EventSource | null = null;
  private listeners: Map<string, EventHandler[]> = new Map();
  private connectionListeners: ConnectionHandler[] = [];
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private filters: Record<string, string> = {};
  private connected = false;
  private lastEventAt = 0;
  private lastErrorAt = 0;

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
    this.connected = false;
    this.emitConnection();

    this.eventSource.onopen = () => {
      this.connected = true;
      this.emitConnection();
    };

    const eventTypes = [
      "job_created", "job_deleted", "job_paused", "job_unpaused",
      "run_started", "run_completed", "run_failed", "run_terminal",
      "task_started", "task_succeeded", "task_failed", "task_skipped", "task_retrying", "task_cached",
      "task_ready", "task_claimed", "task_lease_expired",
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
      this.connected = false;
      this.lastErrorAt = Date.now();
      this.emitConnection();
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

  subscribeConnection(handler: ConnectionHandler) {
    this.connectionListeners.push(handler);
  }

  unsubscribeConnection(handler: ConnectionHandler) {
    this.connectionListeners = this.connectionListeners.filter((listener) => listener !== handler);
  }

  isHealthy() {
    if (this.connected) {
      // If connected but no events for 60s, consider stale (fallback polling kicks in)
      if (this.lastEventAt > 0 && Date.now() - this.lastEventAt > 60000) {
        return false;
      }
      return true;
    }
    return Date.now() - this.lastErrorAt < 10000;
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
    this.connected = false;
    this.emitConnection();
  }

  private emit(event: CaesiumEvent) {
    this.lastEventAt = Date.now();
    const handlers = this.listeners.get(event.type);
    if (handlers) {
      handlers.forEach((h) => h(event));
    }
    // Also emit to wildcard or general listeners if needed
  }

  private emitConnection() {
    this.connectionListeners.forEach((listener) => listener(this.isHealthy()));
  }
}

export const events = new EventManager();
