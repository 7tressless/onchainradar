"use client";

import { typeMeta } from "./bp";
import type { ByType, Stats } from "@/lib/types";

// by_type key → type id, in display order (highest-signal rails first).
const TYPE_ORDER: { k: keyof ByType; type: number }[] = [
  { k: "smart_money", type: 3 },
  { k: "whale", type: 2 },
  { k: "flow", type: 1 },
  { k: "big_borrow", type: 6 },
  { k: "liquidation", type: 5 },
  { k: "lst_flow", type: 4 },
  { k: "depeg", type: 7 },
];

function Bar({
  label,
  segments,
}: {
  label: string;
  segments: { key: string; value: number; color: string }[];
}) {
  const total = segments.reduce((s, x) => s + x.value, 0) || 1;
  return (
    <div className="flex flex-col gap-2">
      <div className="mono text-[10px] uppercase tracking-[0.14em] text-faint">{label}</div>
      <div
        className="flex h-[26px] w-full overflow-hidden border border-line"
        role="img"
        aria-label={`${label}: ${segments.map((s) => `${s.key} ${s.value}`).join(", ")}`}
      >
        {segments.map((s) =>
          s.value > 0 ? (
            <div
              key={s.key}
              className="flex items-center justify-center"
              style={{ width: `${(s.value / total) * 100}%`, background: s.color }}
              title={`${s.key}: ${s.value}`}
            />
          ) : null,
        )}
      </div>
      <div className="flex flex-wrap gap-x-3 gap-y-1">
        {segments.map((s) => (
          <span key={s.key} className="mono flex items-center gap-1 text-[9px] text-muted lg:gap-2 lg:text-[11px]">
            <span className="h-[6px] w-[6px] lg:h-[8px] lg:w-[8px]" style={{ background: s.color }} />
            {s.key}
            <span className="tabular-nums text-faint">{((s.value / total) * 100).toFixed(0)}%</span>
          </span>
        ))}
      </div>
    </div>
  );
}

export function Composition({
  types24,
  typesAll,
}: {
  types24: Stats["last_24h"];
  typesAll: Stats["by_type"];
}) {
  const seg = (src: ByType) =>
    TYPE_ORDER.map((o) => ({
      key: typeMeta(o.type).label,
      value: src[o.k] ?? 0,
      color: typeMeta(o.type).color,
    })).filter((s) => s.value > 0);

  return (
    <div className="flex h-full flex-col justify-center gap-4 px-4 py-3">
      <Bar label="Signal types · 24h" segments={seg(types24)} />
      <Bar label="Signal types · all-time" segments={seg(typesAll)} />
    </div>
  );
}
