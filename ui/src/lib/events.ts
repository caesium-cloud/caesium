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
  private lastEventId: string | null = null;
  private connected = false;
  private opened = false;
  private errorsSinceOpen = 0;
  private lastEventAt = 0;
  private lastErrorAt = 0;

  connect(filters: Record<string, string> = {}) {
    const nextFilters = Object.fromEntries(Object.entries(filters).sort(([a], [b]) => a.localeCompare(b)));
    if (JSON.stringify(nextFilters) !== JSON.stringify(this.filters)) this.lastEventId = null;
    this.filters = nextFilters;
    this.reconnect();
  }

  private reconnect() {
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    if (this.eventSource) {
      this.eventSource.close();
    }

    const params = new URLSearchParams(this.filters);
    if (this.lastEventId) params.set("cursor", this.lastEventId);
    const url = `/v1/events?${params.toString()}`;

    const source = new EventSource(url);
    this.eventSource = source;
    this.connected = false;
    this.emitConnection();

    source.onopen = () => {
      if (this.eventSource !== source) return;
      this.connected = true;
      this.opened = true;
      this.errorsSinceOpen = 0;
      this.emitConnection();
    };

    const eventTypes = [
      "job_created", "job_deleted", "job_paused", "job_unpaused",
      "run_started", "run_retried", "run_completed", "run_failed", "run_cancelled", "run_terminal",
      "task_started", "task_succeeded", "task_failed", "task_skipped", "task_retrying", "task_cached",
      "task_ready", "task_claimed", "task_lease_expired",
      "incident_opened", "incident_status_changed", "agent_action_recorded", "approval_requested",
      "log_chunk"
    ];

    eventTypes.forEach(type => {
      source.addEventListener(type, (message: MessageEvent) => {
        if (this.eventSource !== source) return;
        try {
          const event = JSON.parse(message.data) as CaesiumEvent;
          if (!event.type) event.type = type;
          if (message.lastEventId) this.lastEventId = message.lastEventId;
          this.emit(event);
        } catch (err) {
          console.error(`Failed to parse SSE message for ${type}`, err);
        }
      });
    });

    source.onerror = () => {
      if (this.eventSource !== source) return;
      this.connected = false;
      this.errorsSinceOpen += 1;
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
    // EventSource cannot send an API-key bearer. A stream that never opens,
    // or that keeps erroring, must not stay "healthy" or the console stops polling.
    if (!this.opened || this.errorsSinceOpen >= 3) return false;
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
    this.opened = false;
    this.errorsSinceOpen = 0;
    this.lastEventId = null;
    this.lastEventAt = 0;
    this.lastErrorAt = 0;
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
