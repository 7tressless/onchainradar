"use client";

import { fmtAmount, fmtUsd, shortAddr } from "@/lib/format";

type Affected = { address?: string; label: string };

// Per-type detail rendered from the signal's detector payload (GET /api/signals/{id}).
// Protocol signals (4 lst_flow / 5 liquidation / 6 big_borrow / 7 depeg) have no swap,
// so this fills the hero's "what happened" slot for them.
export function ProtocolDetail({
  type,
  payload,
  sizeUsd,
}: {
  type: number;
  payload: Record<string, unknown>;
  sizeUsd?: string | null;
}) {
  const p = payload ?? {};
  const s = (k: string) => (p[k] == null ? "" : String(p[k]));
  const n = (k: string) => Number(p[k]);

  // 7 depeg: systemic-risk banner (the "risk rail").
  if (type === 7) {
    const sym = s("token_symbol") || s("token") || "STABLE";
    const price = n("implied_price");
    const bps = n("deviation_bps");
    const under = p["under_peg"] === true;
    const affected = (Array.isArray(p["affected_pools"]) ? p["affected_pools"] : []) as Affected[];
    return (
      <div
        className="border border-accent px-3 py-2"
        style={{ backgroundColor: "color-mix(in srgb, var(--color-accent) 6%, transparent)" }}
      >
        <div className="flex items-baseline justify-between gap-2">
          <span className="font-display text-[18px] font-[800] tabular-nums text-accent">
            {sym} {Number.isFinite(price) ? price.toFixed(4) : "—"}
          </span>
          <span className="mono text-[10px] uppercase tracking-[0.08em] text-accent">
            {Number.isFinite(bps) ? `${bps > 0 ? "+" : ""}${bps} bps` : ""} {under ? "under peg" : "off peg"}
          </span>
        </div>
        {affected.length > 0 && (
          <div className="mt-1.5">
            <div className="mono mb-1 text-[9px] uppercase tracking-[0.1em] text-muted">
              {affected.length} pool{affected.length > 1 ? "s" : ""} at risk
            </div>
            <div className="flex flex-wrap gap-1">
              {affected.map((a, i) => (
                <span key={i} className="mono border border-line px-1.5 py-0.5 text-[9px] text-ink">
                  {a.label}
                </span>
              ))}
            </div>
          </div>
        )}
      </div>
    );
  }

  // 5 liquidation: debt repaid, collateral seized, by the liquidator.
  if (type === 5) {
    const coll = s("collateral_asset");
    const debt = s("debt_asset");
    const liquidator = s("liquidator");
    return (
      <div className="mono flex flex-wrap items-center gap-x-2 gap-y-1 text-[12px]">
        <span className="text-muted">repaid</span>
        <span className="font-[700] tabular-nums text-ink">
          {sizeUsd ? fmtUsd(sizeUsd) : "—"}
          {debt ? ` ${debt}` : ""}
        </span>
        <span className="text-accent">→</span>
        <span className="text-muted">seized</span>
        <span className="font-[700] text-ink">{coll || "—"}</span>
        {liquidator && <span className="text-faint">· liquidator {shortAddr(liquidator, 4, 3)}</span>}
      </div>
    );
  }

  // 6 big_borrow: leverage / conviction leading indicator.
  if (type === 6) {
    const reserve = s("reserve");
    const mode = n("interest_rate_mode");
    return (
      <div className="mono flex flex-wrap items-center gap-x-2 gap-y-1 text-[12px]">
        <span className="text-muted">borrowed</span>
        <span className="font-[700] tabular-nums text-ink">
          {sizeUsd ? fmtUsd(sizeUsd) : "—"}
          {reserve ? ` ${reserve}` : ""}
        </span>
        <span className="text-faint">· {mode === 1 ? "stable" : "variable"} rate</span>
      </div>
    );
  }

  // 4 lst_flow: mETH / cmETH money in/out of Mantle.
  if (type === 4) {
    const token = s("token");
    const dir = s("direction");
    const v = s("value_lst");
    const amt = v ? fmtAmount(v, 18) : "";
    const dirColor = dir === "mint" ? "var(--color-up)" : dir === "burn" ? "var(--color-accent)" : "var(--color-muted)";
    const dirNote = dir === "mint" ? "→ into Mantle" : dir === "burn" ? "→ out of Mantle" : "→ wallet move";
    return (
      <div className="mono flex flex-wrap items-center gap-2 text-[12px]">
        <span className="font-[700] tabular-nums text-ink">
          {amt} {token}
        </span>
        <span
          className="px-1.5 py-0.5 text-[9px] uppercase tracking-[0.08em]"
          style={{ color: dirColor, border: `1px solid ${dirColor}` }}
        >
          {dir}
        </span>
        <span className="text-faint">{dirNote}</span>
      </div>
    );
  }

  return null;
}
