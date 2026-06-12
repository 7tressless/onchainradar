"use client";

import { Send } from "lucide-react";
import { useBlockNumber } from "@/lib/hooks";

const SIGNAL_ATTESTOR = "0x7bD664AdfB091E5159fE1CBfa01bFD6f2734A968";
const OUTCOME_ATTESTOR = "0x9161c2A88b219d3FD98CC42D868197824e679a5C";
const SCAN = "https://mantlescan.xyz/address";

// Thin single-row top bar: logo left, a subtle live strip on the right.
export function Chrome({ count }: { count?: number }) {
  const { data: block, isError: chainDown } = useBlockNumber();
  return (
    <header className="flex h-[40px] items-center justify-between border-b border-line px-3 sm:px-6">
      <div className="flex items-center gap-2.5">
        <svg width="20" height="20" viewBox="0 0 22 22" className="flex-shrink-0 text-ink" aria-hidden>
          <rect x="1" y="1" width="20" height="20" fill="none" stroke="currentColor" strokeWidth="1.5" />
          <rect x="6.5" y="6.5" width="9" height="9" fill="none" stroke="currentColor" strokeWidth="1.5" />
          <line x1="11" y1="1" x2="11" y2="6.5" stroke="currentColor" strokeWidth="1.5" />
          <line x1="11" y1="15.5" x2="11" y2="21" stroke="currentColor" strokeWidth="1.5" />
          <circle cx="11" cy="11" r="2" fill="var(--color-accent)" />
        </svg>
        <span className="font-display text-[16px] font-[800] tracking-[0.02em] text-ink">OCR</span>
        <span className="mono hidden h-3 w-px bg-line sm:inline-block" aria-hidden />
        <span className="mono hidden text-[10px] uppercase tracking-[0.04em] text-muted sm:inline">
          OnChain Radar
        </span>
        <span className="hidden items-center gap-1.5 sm:inline-flex">
          <span className="h-[4px] w-[4px] rounded-full bg-accent" />
          <span className="font-display text-[11px] font-[600] tracking-[0.01em] text-ink">Mantle</span>
        </span>
      </div>

      <div className="mono flex items-center gap-3 text-[10px] uppercase tracking-[0.04em] text-faint">
        {/* show "degraded", not a green "live", when the chain read is down */}
        {chainDown ? (
          <span className="flex items-center gap-1.5 text-faint">
            <span className="h-[5px] w-[5px] rounded-full bg-faint" />
            degraded
          </span>
        ) : (
          <span className="flex items-center gap-1.5 text-accent">
            <span
              className="h-[5px] w-[5px] rounded-full bg-accent"
              style={{ animation: "ocr-pulse 1.9s ease-in-out infinite" }}
            />
            live
          </span>
        )}
        <span className="hidden text-line sm:inline">/</span>
        <span className="hidden sm:inline">
          blk <span className="text-ink">{block ? block.toLocaleString("en-US") : "—"}</span>
        </span>
        <span className="text-line">/</span>
        <span>
          <span className="text-ink">{count != null ? count : "—"}</span> attest
        </span>
        <span className="hidden text-line sm:inline">/</span>
        <a data-cursor href={`${SCAN}/${SIGNAL_ATTESTOR}`} target="_blank" rel="noopener noreferrer" className="hidden hover:text-ink sm:inline">
          calls↗
        </a>
        <a data-cursor href={`${SCAN}/${OUTCOME_ATTESTOR}`} target="_blank" rel="noopener noreferrer" className="hidden hover:text-ink sm:inline">
          outcomes↗
        </a>
        <a
          data-cursor
          href="https://t.me/OCRalert"
          target="_blank"
          rel="noopener noreferrer"
          aria-label="OCR Telegram"
          className="flex items-center gap-1 transition-colors hover:opacity-70"
          style={{ color: "var(--color-accent)" }}
        >
          <Send size={12} className="flex-shrink-0" />
          <span className="hidden sm:inline">telegram↗</span>
        </a>
      </div>
    </header>
  );
}
