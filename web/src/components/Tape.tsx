"use client";

import { AnimatePresence, motion, useReducedMotion } from "framer-motion";
import { signalLabel, typeMeta } from "./bp";
import { safeHref } from "./ExtLink";
import { shortAddr } from "@/lib/format";
import type { Signal } from "@/lib/types";

// Bottom ticker: recent on-chain attestations block-by-block, each linking to its tx.
export function Tape({ signals, newIds }: { signals: Signal[]; newIds: Set<number> }) {
  const reduce = useReducedMotion();
  const items = signals.slice(0, 16);

  return (
    <div className="relative z-10 flex h-[34px] flex-shrink-0 items-stretch overflow-hidden border-t border-line">
      <span className="mono flex items-center gap-1.5 px-3 text-[9px] uppercase tracking-[0.18em] text-faint">
        <span
          className="h-[5px] w-[5px] rounded-full bg-accent"
          style={{ animation: "ocr-pulse 1.9s ease-in-out infinite" }}
        />
        live tape
      </span>
      <div className="mono flex min-w-0 flex-1 items-center gap-6 overflow-hidden px-3 text-[10px] uppercase tracking-[0.04em]">
        <AnimatePresence initial={false}>
          {items.map((s) => (
            <motion.a
              key={s.id}
              data-cursor
              href={safeHref(s.attest_url) ?? undefined}
              target="_blank"
              rel="noopener noreferrer"
              layout={!reduce}
              initial={reduce ? false : { opacity: 0, x: -10 }}
              animate={{ opacity: 1, x: 0 }}
              transition={{ duration: 0.22, ease: "easeOut" }}
              className={`flex flex-shrink-0 items-center gap-2 whitespace-nowrap px-2 py-1 text-muted transition-colors duration-150 hover:bg-surface ${
                newIds.has(s.id) ? "ocr-flare" : ""
              }`}
            >
              <span className="text-faint">blk {s.block?.toLocaleString("en-US")}</span>
              <span className="text-ink">{signalLabel(s)}</span>
              <span style={{ color: typeMeta(s.type).color }}>{typeMeta(s.type).label}</span>
              <span>
                {s.type === 3 ? "s" : "z"}
                {(s.score / 100).toFixed(1)}
              </span>
              <span className="text-accent underline decoration-dotted underline-offset-2">
                {shortAddr(s.attest_tx ?? "", 5, 3)} ↗
              </span>
            </motion.a>
          ))}
        </AnimatePresence>
      </div>
    </div>
  );
}
