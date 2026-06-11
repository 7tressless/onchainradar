"use client";

import { ArrowRight } from "lucide-react";
import { LINK } from "./actions";
import { ExtLink } from "./ExtLink";
import { TokenLogo } from "./TokenLogo";
import { fmtAmount } from "@/lib/format";
import type { SwapBody } from "@/lib/types";

// "what the wallet did": token_in → token_out with logos + amounts + dex.
// `swapHref` (the pool's pre-loaded swap UI) makes "on {dex}" a swap link for the
// pair, not just a view of the tx (the tx stays on the hero's SWAP-TX link).
export function SwapAction({ swap, swapHref }: { swap: SwapBody | null; swapHref?: string | null }) {
  if (!swap) return null;
  const onDexHref = swapHref && swapHref.length ? swapHref : swap.tx_url;
  return (
    <div className="mono flex flex-wrap items-center gap-2.5 text-[13px]">
      <span className="flex items-center gap-1.5">
        <TokenLogo url={swap.token_in.logo_url} symbol={swap.token_in.symbol} />
        <span className="font-[700] text-ink tabular-nums">
          {fmtAmount(swap.amount_in_raw, swap.token_in.decimals)}
        </span>
        <span className="text-muted">{swap.token_in.symbol}</span>
      </span>
      <ArrowRight size={15} className="text-accent" />
      <span className="flex items-center gap-1.5">
        <TokenLogo url={swap.token_out.logo_url} symbol={swap.token_out.symbol} />
        <span className="font-[700] text-ink tabular-nums">
          {fmtAmount(swap.amount_out_raw, swap.token_out.decimals)}
        </span>
        <span className="text-muted">{swap.token_out.symbol}</span>
      </span>
      {onDexHref ? (
        <ExtLink
          href={onDexHref}
          className={`${LINK} ml-1`}
          title={
            swapHref && swapHref.length
              ? `Swap ${swap.token_in.symbol} → ${swap.token_out.symbol} on ${swap.dex}`
              : "View this swap on MantleScan"
          }
        >
          on {swap.dex}↗
        </ExtLink>
      ) : (
        <span className="ml-1 text-[10px] uppercase tracking-[0.1em] text-faint">on {swap.dex}</span>
      )}
    </div>
  );
}
