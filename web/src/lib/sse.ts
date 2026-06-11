import { API_BASE } from "./api";
import type { Signal } from "./types";

export type SseStatus = "connecting" | "live" | "reconnecting" | "offline";

export interface SseHandlers {
  onSignal: (s: Signal) => void;
  onStatus?: (status: SseStatus) => void;
}

// Subscribe to /api/live (SSE). The server replays the last 20 signals on connect
// and supports `?last_event_id=` replay on reconnect. EventSource
// can't set request headers, so we pass last_event_id via the query string.
// De-duplication by signal id is the caller's responsibility (replay overlaps).
//
// Browser-only: call this from a useEffect, never during SSR.
export function subscribeLive(handlers: SseHandlers): () => void {
  let es: EventSource | null = null;
  let closed = false;
  let lastId = 0;
  let attempt = 0;
  let timer: ReturnType<typeof setTimeout> | null = null;

  const open = () => {
    if (closed) return;
    handlers.onStatus?.(lastId === 0 ? "connecting" : "reconnecting");
    const url = `${API_BASE}/api/live${lastId ? `?last_event_id=${lastId}` : ""}`;
    es = new EventSource(url);

    es.addEventListener("open", () => {
      attempt = 0;
      handlers.onStatus?.("live");
    });

    es.addEventListener("signal", (ev: MessageEvent<string>) => {
      try {
        const sig = JSON.parse(ev.data) as Signal;
        const evId = Number(ev.lastEventId);
        if (Number.isFinite(evId)) lastId = Math.max(lastId, evId); // guard NaN so replay-from-id keeps working
        handlers.onSignal(sig);
      } catch {
        /* ignore a malformed frame rather than tear down the stream */
      }
    });

    es.addEventListener("error", () => {
      if (closed || !es) return;
      // Reconnect ourselves with backoff + Last-Event-ID replay once the socket
      // is fully closed (the browser's own retry doesn't replay missed ids).
      if (es.readyState === EventSource.CLOSED) {
        es.close();
        es = null;
        const delay = Math.min(1000 * 2 ** attempt, 15000) + Math.random() * 300;
        attempt += 1;
        handlers.onStatus?.("reconnecting");
        timer = setTimeout(open, delay);
      } else {
        handlers.onStatus?.("reconnecting");
      }
    });
  };

  open();

  return () => {
    closed = true;
    if (timer) clearTimeout(timer);
    es?.close();
    es = null;
  };
}
