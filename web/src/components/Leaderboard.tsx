"use client";

import { memo } from "react";
import { scoreColor, typeMeta } from "./bp";
import { ExtLink } from "./ExtLink";
import { shortAddr } from "@/lib/format";
import type { Actor } from "@/lib/types";

// footprint: lowercased actor address → the signal-type ids that wallet has touched
// recently. Multiple colours = a cross-protocol wallet (swapped + borrowed + staked).
export const Leaderboard = memo(function Leaderboard({
  actors,
  footprint,
}: {
  actors: Actor[];
  footprint: Record<string, number[]>;
}) {
  return (
    <div className="flex flex-col">
      <div className="mono grid grid-cols-[28px_1fr_64px_44px_44px] gap-2 border-b border-line px-3 py-1.5 text-[9px] uppercase tracking-[0.1em] text-faint">
        <span>#</span>
        <span>Actor · footprint</span>
        <span className="text-right">Best</span>
        <span className="text-right">Sig</span>
        <span className="text-right">Pools</span>
      </div>
      {actors.map((a, i) => {
        const types = footprint[a.actor.toLowerCase()] ?? [];
        return (
          <ExtLink
            key={a.actor}
            href={a.actor_url}
            className="grid grid-cols-[28px_1fr_64px_44px_44px] items-center gap-2 border-b border-line px-3 py-[7px] transition-colors duration-150 hover:bg-surface"
          >
            <span className="mono text-[11px] font-[700] text-muted">{i + 1}</span>
            <span className="flex min-w-0 items-center gap-1.5">
              <span className="mono truncate text-[11px] text-ink">{shortAddr(a.actor, 6, 4)}</span>
              {types.length > 0 && (
                <span
                  className="flex flex-shrink-0 gap-0.5"
                  title={`Recent: ${types.map((t) => typeMeta(t).label).join(", ")}`}
                >
                  {types.map((t) => (
                    <span key={t} className="h-[6px] w-[6px]" style={{ background: typeMeta(t).color }} />
                  ))}
                </span>
              )}
            </span>
            <span
              className="mono text-right text-[13px] font-[700] tabular-nums"
              style={{ color: scoreColor(a.best_score * 100) }}
            >
              {a.best_score.toFixed(0)}
            </span>
            <span className="mono text-right text-[12px] tabular-nums text-muted">{a.total_signals}</span>
            <span className="mono text-right text-[12px] tabular-nums text-muted">{a.pools_touched_24h}</span>
          </ExtLink>
        );
      })}
    </div>
  );
});
