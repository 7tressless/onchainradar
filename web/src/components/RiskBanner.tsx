"use client";

import { AlertTriangle } from "lucide-react";
import { useSignalDetail } from "@/lib/hooks";
import type { Signal } from "@/lib/types";

// Surfaces an active stablecoin depeg above the fold so it isn't lost in the feed.
// Mounted (and fetches detail) only when a depeg signal exists; click to inspect.
export function RiskBanner({ signal, onInspect }: { signal: Signal; onInspect: () => void }) {
  const { data: detail } = useSignalDetail(signal.id);
  const p = (detail?.payload ?? {}) as Record<string, unknown>;
  const sym = p.token_symbol ? String(p.token_symbol) : "";
  const price = Number(p.implied_price);
  const bps = Number(p.deviation_bps);
  const pools = Array.isArray(p.affected_pools) ? p.affected_pools.length : 0;

  return (
    <button
      data-cursor
      onClick={onInspect}
      aria-live="polite"
      className="mono flex w-full items-center gap-2 border-b border-accent px-6 py-1.5 text-left text-[11px] uppercase tracking-[0.08em] text-accent"
      style={{ backgroundColor: "color-mix(in srgb, var(--color-accent) 8%, transparent)" }}
    >
      <AlertTriangle size={13} className="flex-shrink-0" style={{ animation: "ocr-pulse 1.6s ease-in-out infinite" }} />
      <span className="font-[700]">Stablecoin depeg detected</span>
      {sym && Number.isFinite(price) && (
        <span>
          · {sym} {price.toFixed(4)}
        </span>
      )}
      {Number.isFinite(bps) && (
        <span>
          · {bps > 0 ? "+" : ""}
          {bps} bps
        </span>
      )}
      {pools > 0 && (
        <span>
          · {pools} pool{pools > 1 ? "s" : ""} at risk
        </span>
      )}
      <span className="ml-auto opacity-70">inspect →</span>
    </button>
  );
}
