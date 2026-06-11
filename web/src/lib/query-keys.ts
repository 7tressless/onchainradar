import type { SignalTypeId } from "./types";

// Centralised TanStack Query keys so invalidation stays consistent.
export const qk = {
  stats: ["stats"] as const,
  signals: (p?: { limit?: number; type?: SignalTypeId; pool?: string; actor?: string; graded?: boolean }) =>
    ["signals", p ?? {}] as const,
  signal: (id: number) => ["signal", id] as const,
  actors: (limit?: number) => ["actors", limit ?? null] as const,
  pools: ["pools"] as const,
  series: (pool: string, metric: string, hours: number) =>
    ["series", pool, metric, hours] as const,
};
