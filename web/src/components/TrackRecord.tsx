"use client";

import { memo } from "react";
import { LINK } from "./actions";
import { GRADE_COLOR, scoreColor, signalLabel, TYPE_ICON, typeMeta } from "./bp";
import { ExtLink } from "./ExtLink";
import { RelTime } from "./RelTime";
import { TokenLogo } from "./TokenLogo";
import type { PoolLogos, Signal } from "@/lib/types";

const COLS = "grid-cols-[48px_52px_1fr_54px_28px_64px]";

export const TrackRecord = memo(function TrackRecord({ signals, logos }: { signals: Signal[]; logos: PoolLogos }) {
  return (
    <div className="flex flex-col">
      <div className={`mono sticky top-0 z-10 grid ${COLS} gap-2 border-b border-line bg-bg px-3 py-1.5 text-[9px] uppercase tracking-[0.1em] text-faint`}>
        <span>Time</span>
        <span>Type</span>
        <span>Pool</span>
        <span className="text-right">Score</span>
        <span className="text-center">Gr</span>
        <span className="text-right">Proof</span>
      </div>
      {signals.map((s) => {
        const lg = logos[(s.pool ?? "").toLowerCase()];
        const Icon = TYPE_ICON[s.type];
        const meta = typeMeta(s.type);
        return (
          <div key={s.id} className={`grid ${COLS} items-center gap-2 border-b border-line px-3 py-[7px]`}>
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
            <span className="mono text-right text-[12px] font-[700] tabular-nums" style={{ color: scoreColor(s.score) }}>
              {(s.score / 100).toFixed(1)}
            </span>
            <span className="text-center">
              {s.outcome ? (
                <span
                  className="font-display text-[16px] font-[800] leading-none"
                  style={{ color: GRADE_COLOR[s.outcome.letter] }}
                  title={`grade ${s.outcome.letter} · ${s.outcome.status}`}
                >
                  {s.outcome.letter}
                </span>
              ) : (
                <span className="text-faint">·</span>
              )}
            </span>
            <span className="mono flex items-center justify-end gap-2 text-[10px]">
              {s.attest_url && (
                <ExtLink href={s.attest_url} className={LINK} title="attestation">
                  att↗
                </ExtLink>
              )}
              {s.outcome?.attest_url && (
                <ExtLink href={s.outcome.attest_url} className={LINK} title="graded outcome">
                  out↗
                </ExtLink>
              )}
            </span>
          </div>
        );
      })}
    </div>
  );
});
