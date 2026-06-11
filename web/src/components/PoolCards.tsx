"use client";

import { memo } from "react";
import { BTN } from "./actions";
import { changeColor, Sparkline } from "./bp";
import { ExtLink } from "./ExtLink";
import { TokenLogo } from "./TokenLogo";
import { useSeries } from "@/lib/hooks";
import { fmtUsd } from "@/lib/format";
import type { Pool } from "@/lib/types";

const DEX_LABEL: Record<string, string> = { agni: "AGNI", merchant_moe: "MOE", fusionx: "FUSIONX" };
function dexLabel(dex: string): string {
  return DEX_LABEL[dex] ?? dex.replace(/_/g, " ").toUpperCase();
}

function Mini({ k, v }: { k: string; v: string }) {
  return (
    <div className="flex flex-col">
      <span className="text-[9px] uppercase tracking-[0.06em] text-faint">{k}</span>
      <span className="text-[10px] tabular-nums text-ink">{v}</span>
    </div>
  );
}

function PoolCard({ pool }: { pool: Pool }) {
  const { data: series } = useSeries(pool.address, "swap_count", 24);
  const points = (series?.points ?? []).map((p) => Number(p.value));
  const price = pool.stats?.price_usd ? Number(pool.stats.price_usd) : null;
  const chg = pool.stats?.price_change_24h != null ? Number(pool.stats.price_change_24h) : null;
  const cc = changeColor(chg);
  const dex = dexLabel(pool.dex);

  return (
    <div className="flex flex-1 min-w-0 flex-col gap-1.5 border border-line px-3 py-2.5 lg:border-y-0 lg:border-l-0 lg:border-r lg:last:border-r-0">
      <div className="flex items-center justify-between">
        <div className="flex min-w-0 items-center gap-1.5">
          <span className="flex flex-shrink-0 -space-x-1">
            <TokenLogo url={pool.token0_logo_url} symbol={pool.token0_symbol} size={15} />
            <TokenLogo url={pool.token1_logo_url} symbol={pool.token1_symbol} size={15} />
          </span>
          <span className="font-display truncate text-[12px] font-[700] uppercase">{pool.label}</span>
        </div>
        <span className="mono text-[9px] uppercase text-faint">{dex}</span>
      </div>

      <div className="flex items-end justify-between gap-2">
        <div>
          <div className="font-display text-[16px] font-[800] tabular-nums leading-none">
            {price != null
              ? `$${price < 2 ? price.toFixed(4) : price.toLocaleString("en-US", { maximumFractionDigits: 2 })}`
              : "—"}
          </div>
          <div className="mono mt-0.5 text-[10px] tabular-nums" style={{ color: cc }}>
            {chg != null ? `${chg >= 0 ? "+" : ""}${chg.toFixed(2)}%` : "—"}
            <span className="text-faint"> 24h</span>
          </div>
        </div>
        <Sparkline data={points} w={70} h={22} stroke={cc} />
      </div>

      <div className="mono grid grid-cols-3 gap-1.5 border-t border-line pt-1.5">
        <Mini k="TVL" v={pool.stats ? fmtUsd(pool.stats.tvl_usd) : "—"} />
        <Mini k="Vol 24h" v={pool.stats ? fmtUsd(pool.stats.vol24h_usd) : "—"} />
        <Mini k="Sig 24h" v={`${pool.recent_signals_24h}`} />
      </div>

      <div className="mono mt-0.5 flex flex-col gap-1 border-t border-line pt-1.5 text-[9px] uppercase tracking-[0.04em] lg:flex-row">
        {pool.trade_url && (
          <ExtLink
            href={pool.trade_url}
            className={`${BTN} w-full lg:flex-1`}
            title="Swap this pair on Matcha (gasless, best route across Mantle DEXs)"
          >
            swap ↗
          </ExtLink>
        )}
        {(pool.lp_url || pool.dex_url) && (
          <ExtLink
            href={pool.lp_url?.length ? pool.lp_url : pool.dex_url}
            className={`${BTN} w-full lg:flex-1`}
            title={pool.lp_url?.length ? "Add liquidity to this pool" : "Add liquidity on the native DEX"}
          >
            liquidity ↗
          </ExtLink>
        )}
      </div>
    </div>
  );
}

const MIN_VOL_USD = 20_000;

export const PoolCards = memo(function PoolCards({ pools }: { pools: Pool[] }) {
  // Show only liquid/active pools: rank by 24h volume, keep those above a floor OR
  // with recent signals, take the top 6. Illiquid pairs are dropped from this view.
  const ranked = pools
    .map((p) => ({ p, vol: p.stats ? Number(p.stats.vol24h_usd) || 0 : 0 }))
    .sort((a, b) => b.vol - a.vol);
  const active = ranked.filter((x) => x.vol >= MIN_VOL_USD || x.p.recent_signals_24h > 0);
  const shown = (active.length ? active : ranked).slice(0, 6).map((x) => x.p);
  return (
    <div className="grid grid-cols-2 gap-2 sm:grid-cols-3 lg:flex lg:gap-0">
      {shown.map((p) => (
        <PoolCard key={p.address} pool={p} />
      ))}
    </div>
  );
});
