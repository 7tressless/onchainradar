"use client";

import { useSyncExternalStore } from "react";
import { relTime } from "@/lib/format";

// One shared 1s ticker for all relative-time labels. Each <RelTime/> subscribes
// individually, so a tick re-renders only the time spans, not their parent rows.
let now = Date.now();
const subs = new Set<() => void>();
let timer: ReturnType<typeof setInterval> | null = null;

function subscribe(cb: () => void): () => void {
  subs.add(cb);
  if (!timer) {
    timer = setInterval(() => {
      now = Date.now();
      subs.forEach((f) => f());
    }, 1000);
  }
  return () => {
    subs.delete(cb);
    if (subs.size === 0 && timer) {
      clearInterval(timer);
      timer = null;
    }
  };
}

// getServerSnapshot returns 0 so SSR and the hydration pass both render an empty
// string (matching); the client then re-renders with the live value. No mismatch.
function useNow(): number {
  return useSyncExternalStore(
    subscribe,
    () => now,
    () => 0,
  );
}

export function RelTime({ iso, className }: { iso: string; className?: string }) {
  const n = useNow();
  return (
    <span className={className} suppressHydrationWarning>
      {n ? relTime(iso, n) : ""}
    </span>
  );
}
