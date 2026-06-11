import type {
  Actor,
  Metric,
  Pool,
  Series,
  SignalDetail,
  SignalTypeId,
  SignalsPage,
  Stats,
} from "./types";

// Same-origin by default: "" means the browser hits /api/* on the same host and the
// Next rewrite proxies to the backend (next.config.mjs). NEXT_PUBLIC_API_BASE overrides
// for direct-API mode.
export const API_BASE = (process.env.NEXT_PUBLIC_API_BASE ?? "").replace(/\/+$/, "");

type QueryParams = Record<string, string | number | boolean | undefined>;

function qs(params?: QueryParams): string {
  if (!params) return "";
  const pairs = Object.entries(params).filter(
    ([, v]) => v !== undefined && v !== null && v !== "",
  );
  if (pairs.length === 0) return "";
  return `?${pairs
    .map(([k, v]) => `${k}=${encodeURIComponent(String(v))}`)
    .join("&")}`;
}

async function get<T>(path: string, signal?: AbortSignal): Promise<T> {
  const res = await fetch(`${API_BASE}${path}`, {
    headers: { Accept: "application/json" },
    signal,
  });
  if (!res.ok) {
    // The API returns { error } JSON on failures; surface it, never swallow.
    let message = `HTTP ${res.status}`;
    try {
      const body = (await res.json()) as { error?: string };
      if (body?.error) message = body.error;
    } catch {
      /* non-JSON error body */
    }
    throw new Error(message);
  }
  return (await res.json()) as T;
}

export const api = {
  stats: (signal?: AbortSignal) => get<Stats>("/api/stats", signal),
  signals: (
    params?: {
      limit?: number;
      offset?: number;
      type?: SignalTypeId;
      pool?: string;
      actor?: string;
      graded?: boolean;
    },
    signal?: AbortSignal,
  ) => get<SignalsPage>(`/api/signals${qs(params)}`, signal),
  signal: (id: number, signal?: AbortSignal) =>
    get<SignalDetail>(`/api/signals/${id}`, signal),
  actors: (limit?: number, signal?: AbortSignal) =>
    get<{ items: Actor[] }>(`/api/actors${qs({ limit })}`, signal),
  pools: (signal?: AbortSignal) => get<{ items: Pool[] }>("/api/pools", signal),
  series: (pool: string, metric: Metric, hours = 24, signal?: AbortSignal) =>
    get<Series>(`/api/series${qs({ pool, metric, hours })}`, signal),
};
