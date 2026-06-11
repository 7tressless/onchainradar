"use client";

import { GRADE_COLOR } from "./bp";
import type { GradeDistribution } from "@/lib/types";

const ORDER: (keyof GradeDistribution)[] = ["A", "B", "C", "D", "F"];

function arc(cx: number, cy: number, r: number, a0: number, a1: number, width: number) {
  const p = (a: number, rad: number) => [cx + rad * Math.cos(a), cy + rad * Math.sin(a)];
  const ro = r;
  const ri = r - width;
  const [x0o, y0o] = p(a0, ro);
  const [x1o, y1o] = p(a1, ro);
  const [x0i, y0i] = p(a1, ri);
  const [x1i, y1i] = p(a0, ri);
  const large = a1 - a0 > Math.PI ? 1 : 0;
  return `M${x0o} ${y0o} A${ro} ${ro} 0 ${large} 1 ${x1o} ${y1o} L${x0i} ${y0i} A${ri} ${ri} 0 ${large} 0 ${x1i} ${y1i} Z`;
}

export function GradeDonut({
  grades,
  unrated = 0,
}: {
  grades: GradeDistribution;
  unrated?: number;
}) {
  const total = ORDER.reduce((s, k) => s + grades[k], 0) || 1;
  const ab = ((grades.A + grades.B) / total) * 100;
  let a = -Math.PI / 2;
  const segs = ORDER.map((k) => {
    const frac = grades[k] / total;
    const a0 = a;
    const a1 = a + frac * Math.PI * 2;
    a = a1;
    return { k, a0: a0 + 0.01, a1: a1 - 0.01, color: GRADE_COLOR[k] };
  });

  return (
    <div className="flex h-full items-center justify-center gap-6 px-4 py-3 sm:gap-8">
      <div className="relative flex-shrink-0">
        <svg
          viewBox="0 0 118 118"
          className="h-[140px] w-[140px] lg:h-[156px] lg:w-[156px]"
          role="img"
          aria-label={`Grade distribution: ${ORDER.map((k) => `${k} ${grades[k]}`).join(", ")} · A/B ${ab.toFixed(0)}%`}
        >
          {segs.map((s) =>
            s.a1 > s.a0 ? <path key={s.k} d={arc(59, 59, 54, s.a0, s.a1, 16)} fill={s.color} /> : null,
          )}
        </svg>
        <div className="pointer-events-none absolute inset-0 flex flex-col items-center justify-center">
          <span className="font-display text-[26px] font-[800] leading-none tabular-nums">
            {ab.toFixed(0)}%
          </span>
          <span className="mono text-[9px] uppercase tracking-[0.14em] text-faint">A / B</span>
        </div>
      </div>
      <div className="flex flex-col gap-1.5">
        {ORDER.map((k) => (
          <div key={k} className="mono flex items-center gap-2 text-[11px]">
            <span className="h-[8px] w-[8px]" style={{ background: GRADE_COLOR[k] }} />
            <span className="w-3 text-ink">{k}</span>
            <span className="tabular-nums text-muted">{grades[k]}</span>
            <span className="tabular-nums text-faint">
              {((grades[k] / total) * 100).toFixed(0)}%
            </span>
          </div>
        ))}
        <div className="mono mt-1 text-[10px] text-faint">
          <span className="tabular-nums text-muted">{total}</span> of{" "}
          <span className="tabular-nums text-muted">{total + unrated}</span> graded
        </div>
      </div>
    </div>
  );
}
