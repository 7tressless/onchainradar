"use client";

import { useQuery } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { api } from "./api";
import { fxActors, fxPools, fxStats, subscribeLiveFixtures, USE_FIXTURES } from "./fixtures";
import { qk } from "./query-keys";
import { subscribeLive, type SseStatus } from "./sse";
import type { Metric, Signal } from "./types";

export function useStats() {
  return useQuery({
    queryKey: qk.stats,
    queryFn: ({ signal }) => (USE_FIXTURES ? Promise.resolve(fxStats()) : api.stats(signal)),
    refetchInterval: 10_000,
  });
}

export function usePools() {
  return useQuery({
    queryKey: qk.pools,
    queryFn: ({ signal }) =>
      USE_FIXTURES ? Promise.resolve({ items: fxPools() }) : api.pools(signal),
    staleTime: 60_000,
  });
}

export function useActors(limit = 5) {
  return useQuery({
    queryKey: qk.actors(limit),
    queryFn: ({ signal }) =>
      USE_FIXTURES ? Promise.resolve({ items: fxActors() }) : api.actors(limit, signal),
    staleTime: 15_000,
    refetchInterval: 20_000, // poll so new smart-money actors / scores appear without a reload
  });
}

// Recent signals from the enriched JSON API (note, grade, links, swap-tx).
export function useSignals(limit = 60, graded = false) {
  return useQuery({
    queryKey: qk.signals({ limit, graded }), // limit + graded in the key so callers don't share a cache entry
    queryFn: ({ signal }) => api.signals({ limit, graded: graded || undefined }, signal),
    refetchInterval: 8000,
    retry: 1, // avoid stacking retries inside the 8s poll window during an outage
  });
}

// Per-pool sparkline series. A 24h shape barely moves, so a 5-min staleTime avoids
// refetching it every time a pool card remounts.
export function useSeries(pool: string, metric: Metric = "swap_count", hours = 24) {
  return useQuery({
    queryKey: qk.series(pool, metric, hours),
    queryFn: ({ signal }) => api.series(pool, metric, hours, signal),
    staleTime: 300_000,
    gcTime: 600_000,
    enabled: !!pool,
  });
}

// Full detail for one signal, including the decoded swap action.
export function useSignalDetail(id: number | undefined) {
  return useQuery({
    queryKey: qk.signal(id ?? -1),
    queryFn: ({ signal }) => api.signal(id as number, signal),
    enabled: id != null,
    staleTime: 60_000, // detail is immutable once enriched, so don't refetch on re-select
  });
}

const FEED_CAP = 60;
const FLARE_MS = 6000; // how long a just-arrived row stays visually flared

// Source-agnostic live feed: real SSE or the fixtures simulation. De-dups by id,
// newest-first, capped. Returns connection status and the set of just-arrived ids
// (for the live flare). Signals from the connect-time replay are not flared.
export function useLiveFeed() {
  const [signals, setSignals] = useState<Signal[]>([]);
  const [status, setStatus] = useState<SseStatus>("connecting");
  const [newIds, setNewIds] = useState<Set<number>>(new Set());
  const seen = useRef<Set<number>>(new Set());
  const primed = useRef(false);
  const timers = useRef<Map<number, ReturnType<typeof setTimeout>>>(new Map());

  useEffect(() => {
    // The first ~1.5s of signals are the connect-time replay (history, not "new").
    const primeT = setTimeout(() => {
      primed.current = true;
    }, 1500);
    const handlers = {
      onSignal: (s: Signal) => {
        if (seen.current.has(s.id)) return;
        seen.current.add(s.id);
        setSignals((prev) => {
          const next = [s, ...prev].slice(0, FEED_CAP);
          // Keep `seen` bounded; over days of uptime it would otherwise grow forever.
          if (seen.current.size > FEED_CAP * 4) seen.current = new Set(next.map((x) => x.id));
          return next;
        });
        if (!primed.current) return;
        setNewIds((prev) => new Set(prev).add(s.id));
        const t = setTimeout(() => {
          setNewIds((prev) => {
            const n = new Set(prev);
            n.delete(s.id);
            return n;
          });
          timers.current.delete(s.id);
        }, FLARE_MS);
        timers.current.set(s.id, t);
      },
      onStatus: setStatus,
    };
    const unsubscribe = USE_FIXTURES
      ? subscribeLiveFixtures(handlers)
      : subscribeLive(handlers);
    const liveTimers = timers.current;
    return () => {
      clearTimeout(primeT);
      liveTimers.forEach((t) => clearTimeout(t));
      liveTimers.clear();
      unsubscribe();
    };
  }, []);

  return { signals, status, newIds };
}

// On-chain data via same-origin Next route (server-side viem).

export function useBlockNumber() {
  return useQuery({
    queryKey: ["chain", "block"],
    queryFn: async () => {
      const r = await fetch("/api/chain/block");
      if (!r.ok) throw new Error("block read failed");
      return (await r.json()).block as number;
    },
    refetchInterval: 2000,
    retry: 0, // a 2s poller must not stack retries on top of itself when the RPC is down
  });
}

// Recent on-chain attestations + the set of just-arrived ids (for the live pulse).
export function useChainSignals() {
  const seen = useRef<Set<number>>(new Set());
  const [newIds, setNewIds] = useState<Set<number>>(new Set());
  const timers = useRef<Map<number, ReturnType<typeof setTimeout>>>(new Map());

  const q = useQuery({
    queryKey: ["chain", "signals"],
    queryFn: async () => {
      const r = await fetch("/api/chain/signals");
      if (!r.ok) {
        const j = (await r.json().catch(() => ({}))) as { error?: string };
        throw new Error(j.error ?? "chain read failed");
      }
      return (await r.json()).signals as Signal[];
    },
    refetchInterval: 5000,
    retry: 1,
  });

  useEffect(() => {
    if (!q.data) return;
    const fresh: number[] = [];
    for (const s of q.data) {
      if (!seen.current.has(s.id)) {
        fresh.push(s.id);
        seen.current.add(s.id);
      }
    }
    // Keep `seen` bounded over long uptime (chain poll runs every 5s for weeks).
    if (seen.current.size > 300) seen.current = new Set(q.data.map((s) => s.id));
    // Skip the initial bulk load; only pulse genuinely new arrivals. Per-id timers
    // live in a ref (not tied to q.data identity) so they neither leak nor clear early.
    if (!fresh.length || seen.current.size === fresh.length) return;
    setNewIds((prev) => new Set([...prev, ...fresh]));
    for (const id of fresh) {
      const t = setTimeout(() => {
        setNewIds((prev) => {
          const n = new Set(prev);
          n.delete(id);
          return n;
        });
        timers.current.delete(id);
      }, FLARE_MS);
      timers.current.set(id, t);
    }
  }, [q.data]);

  // Clean up any in-flight flare timers on unmount.
  useEffect(() => {
    const liveTimers = timers.current;
    return () => {
      liveTimers.forEach((t) => clearTimeout(t));
      liveTimers.clear();
    };
  }, []);

  return {
    signals: q.data ?? [],
    isLoading: q.isLoading,
    isError: q.isError,
    error: q.error as Error | null,
    refetch: q.refetch,
    newIds,
  };
}
