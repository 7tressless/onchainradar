"use client";

import { memo } from "react";
import { scoreColor, signalLabel, TYPE_ICON, typeMeta } from "./bp";
import { RelTime } from "./RelTime";
import { TokenLogo } from "./TokenLogo";
import { EmptyState } from "./states";
import { fmtUsd, shortAddr } from "@/lib/format";
import type { SseStatus } from "@/lib/sse";
import type { PoolLogos, Signal } from "@/lib/types";

const COLS = "grid-cols-[42px_48px_minmax(0,1fr)_50px_46px_54px]";

// memo: the dashboard re-renders on every poll/SSE tick, but this list re-renders
// only when its own (already-memoized) props actually change.
export const LiveFeed = memo(function LiveFeed({
  signals,
  status,
  newIds,
  selectedId,
  onSelect,
  logos,
}: {
  signals: Signal[];
  status: SseStatus;
  newIds: Set<number>;
  selectedId?: number;
  onSelect: (id: number) => void;
  logos: PoolLogos;
}) {
  if (signals.length === 0) {
    const title =
      status === "live"
        ? "Live · waiting for the next signal"
        : status === "offline"
          ? "Live stream offline"
          : "Connecting to the live stream…";
    return (
      <EmptyState
        title={title}
        sub={status === "offline" ? "Retrying automatically." : "The agent streams every new signal here in real time."}
      />
    );
  }

  return (
    <div className="flex flex-col">
      <div className={`mono sticky top-0 z-10 grid ${COLS} gap-1.5 border-b border-line bg-bg px-3 py-1.5 text-[9px] uppercase tracking-[0.08em] text-faint`}>
        <span>Time</span>
        <span>Type</span>
        <span>Pool</span>
        <span className="text-right">Size</span>
        <span className="text-right">Score</span>
        <span>Actor</span>
      </div>
      {signals.map((s) => {
        const lg = logos[(s.pool ?? "").toLowerCase()];
        const Icon = TYPE_ICON[s.type];
        const meta = typeMeta(s.type);
        const fresh = newIds.has(s.id);
        return (
          <button
            key={s.id}
            data-cursor
            onClick={() => onSelect(s.id)}
            className={`grid ${COLS} items-center gap-1.5 border-b border-line px-3 py-[7px] text-left transition-colors duration-150 hover:bg-surface ${
              s.id === selectedId ? "bg-surface" : ""
            } ${fresh ? "ocr-flare" : ""}`}
          >
            <RelTime iso={s.created_at} className="mono text-[10px] text-faint" />
            <span className="mono text-[10px] uppercase" style={{ color: meta.color }}>
              {meta.label}
            </span>
            <span className="flex min-w-0 items-center gap-1.5">
              {lg ? (
                <span className="flex flex-shrink-0 -space-x-1">
                  <TokenLogo url={lg.l0} symbol="" size={14} />
                  <TokenLogo url={lg.l1} symbol="" size={14} />
                </span>
              ) : Icon ? (
                <Icon size={13} className="flex-shrink-0" style={{ color: meta.color }} />
              ) : null}
              <span className="font-display truncate text-[12px] font-[600] uppercase">{signalLabel(s)}</span>
            </span>
            <span className="mono text-right text-[10px] tabular-nums text-muted">
              {s.size_usd ? fmtUsd(s.size_usd) : <span className="text-faint">·</span>}
            </span>
            <span className="mono text-right text-[13px] font-[700] tabular-nums" style={{ color: scoreColor(s.score) }}>
              {(s.score / 100).toFixed(1)}
            </span>
            <span className="mono truncate text-[10px] text-muted">{s.actor ? shortAddr(s.actor, 4, 3) : "—"}</span>
          </button>
        );
      })}
    </div>
  );
});
