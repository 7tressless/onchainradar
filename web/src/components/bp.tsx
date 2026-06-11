"use client";

// Shared "blueprint" primitives + style helpers.
import type { ReactNode } from "react";
import { AlertTriangle, Coins, TrendingUp, Zap, type LucideIcon } from "lucide-react";
import { PanelBoundary } from "./PanelBoundary";

export function Panel({
  title,
  right,
  children,
  className = "",
  bodyClassName = "",
  resetKey,
}: {
  title?: string;
  right?: ReactNode;
  children: ReactNode;
  className?: string;
  bodyClassName?: string;
  resetKey?: unknown; // changes on each data tick → the error boundary auto-recovers
}) {
  return (
    <section className={`flex min-h-0 flex-col border border-line bg-bg ${className}`}>
      {title && (
        <div className="flex flex-shrink-0 items-center justify-between border-b border-line px-3 py-2">
          <span className="mono text-[10px] uppercase tracking-[0.16em] text-ink">{title}</span>
          {right && <span className="mono text-[9px] uppercase tracking-[0.12em] text-faint">{right}</span>}
        </div>
      )}
      <div className={`min-h-0 flex-1 ${bodyClassName}`}>
        <PanelBoundary resetKey={resetKey}>{children}</PanelBoundary>
      </div>
    </section>
  );
}

// Per-type colours (defined as tokens in globals.css). Hue groups the rails:
// slate/ochre/sage = DEX, blue = LST, terracotta/brick = lending, vermilion = depeg.
export const TYPE_META: Record<number, { label: string; color: string }> = {
  1: { label: "FLOW", color: "var(--type-flow)" },
  2: { label: "WHALE", color: "var(--type-whale)" },
  3: { label: "SMART", color: "var(--type-smart)" },
  4: { label: "LST", color: "var(--type-lst)" },
  5: { label: "LIQ", color: "var(--type-liq)" },
  6: { label: "BORROW", color: "var(--type-borrow)" },
  7: { label: "DEPEG", color: "var(--color-accent)" }, // systemic alarm
};

const TYPE_FALLBACK = { label: "OTHER", color: "var(--color-muted)" };

// Safe accessor; never crashes on an unknown or future type id.
export function typeMeta(t: number): { label: string; color: string } {
  return TYPE_META[t] ?? TYPE_FALLBACK;
}

// Human label for the hero / lists when there is no DEX pair label.
const TYPE_LONG: Record<number, string> = {
  1: "Flow anomaly",
  2: "Whale swap",
  3: "Smart money",
  4: "LST flow",
  5: "Liquidation",
  6: "Big borrow",
  7: "Depeg",
};

export function typeLong(t: number): string {
  return TYPE_LONG[t] ?? "Signal";
}

// DEX signals (1/2/3) carry a pool pair label; protocol signals (4-7) don't, so
// fall back to a humanized type name to keep the label cell from going blank.
export function signalLabel(s: { pool_label: string; type: number; type_name?: string }): string {
  if (s.pool_label && s.pool_label.trim()) return s.pool_label.split(" · ")[0];
  return TYPE_LONG[s.type] ?? (s.type_name ? s.type_name.replace(/_/g, " ") : "—");
}

// Glyph for protocol signals (which have no token logos); marks the new rails.
export const TYPE_ICON: Record<number, LucideIcon> = {
  4: Coins,
  5: Zap,
  6: TrendingUp,
  7: AlertTriangle,
};

// Grade ramp: sage → dusty-blue → ochre → terracotta → vermilion.
export const GRADE_COLOR: Record<string, string> = {
  A: "var(--grade-a)",
  B: "var(--grade-b)",
  C: "var(--grade-c)",
  D: "var(--grade-d)",
  F: "var(--color-accent)",
};

// Up = sage, down = vermilion.
export function changeColor(chg: number | null): string {
  if (chg == null) return "var(--color-faint)";
  return chg >= 0 ? "var(--color-up)" : "var(--color-accent)";
}

// Score colour: red only when hot (z≥100 or capped / high accumulation).
export function scoreColor(score: number): string {
  return score >= 10000 ? "var(--color-accent)" : "var(--color-ink)";
}

export function Sparkline({
  data,
  w = 104,
  h = 30,
  stroke = "var(--color-ink)",
}: {
  data: number[];
  w?: number;
  h?: number;
  stroke?: string;
}) {
  if (data.length < 2) return <svg width={w} height={h} aria-hidden />;
  const max = Math.max(...data);
  const min = Math.min(...data);
  const span = max - min || 1;
  const pts = data
    .map((v, i) => `${((i / (data.length - 1)) * w).toFixed(1)},${(h - ((v - min) / span) * (h - 2) - 1).toFixed(1)}`)
    .join(" ");
  return (
    <svg width={w} height={h} viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none" className="block">
      <polyline
        points={pts}
        fill="none"
        stroke={stroke}
        strokeWidth="1.25"
        vectorEffect="non-scaling-stroke"
        strokeLinejoin="round"
      />
    </svg>
  );
}
