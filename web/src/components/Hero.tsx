"use client";

import { motion, useReducedMotion } from "framer-motion";
import { BTN, LINK } from "./actions";
import { signalLabel, typeLong, typeMeta } from "./bp";
import { ExtLink } from "./ExtLink";
import { ProtocolDetail } from "./ProtocolDetail";
import { RelTime } from "./RelTime";
import { SwapAction } from "./SwapAction";
import { useSignalDetail } from "@/lib/hooks";
import { fmtUsd, parseNote } from "@/lib/format";
import type { Signal } from "@/lib/types";

function Lnk({ label, href }: { label: string; href: string }) {
  return (
    <ExtLink href={href} className={LINK}>
      {label}↗
    </ExtLink>
  );
}

export function Hero({ signal, swapHref }: { signal?: Signal; swapHref?: string | null }) {
  const reduce = useReducedMotion();
  const { data: detail } = useSignalDetail(signal?.id);
  if (!signal) return <div className="h-full" />;

  const meta = typeMeta(signal.type);
  const mag = signal.score / 100;
  const hot = signal.type === 7 || (signal.type !== 1 && mag >= 100);
  const { text: noteText, confidence } = parseNote(signal.note);
  const swap = detail?.swap ?? null;
  const isProtocol = signal.type >= 4; // 4 lst / 5 liq / 6 borrow / 7 depeg
  const label = signalLabel(signal);
  const headline = label.includes("/") ? label.split("/").join(" / ") : label;
  const scoreKind = signal.type === 3 ? "accum" : signal.type <= 2 ? "z-score" : "score";
  // The triggering tx means different things per rail; label it accordingly.
  const txLabel =
    signal.type <= 3 ? "swap-tx" : signal.type === 5 ? "liq-tx" : signal.type === 6 ? "borrow-tx" : signal.type === 4 ? "flow-tx" : "tx";
  // "Go interact" CTA: short inline label; the backend's action_label is the tooltip.
  const actionLabel = signal.type === 6 ? "borrow" : signal.type === 5 ? "view" : signal.type === 4 ? "stake" : "open";

  return (
    <motion.div
      key={signal.id}
      initial={reduce ? false : { opacity: 0, y: 5 }}
      animate={reduce ? undefined : { opacity: 1, y: 0 }}
      transition={{ duration: 0.22, ease: "easeOut" }}
      className="flex h-full flex-col px-4 py-3"
    >
      <div className="mono mb-2 flex flex-shrink-0 items-center justify-between text-[9px] uppercase tracking-[0.14em] text-faint">
        <span className="flex items-center gap-1.5">
          Latest ·{" "}
          <span className="font-[700]" style={{ color: meta.color }}>
            {typeLong(signal.type)}
          </span>{" "}
          · <RelTime iso={signal.created_at} />
        </span>
        {confidence && <span className="text-accent">conf {confidence}</span>}
      </div>

      <div className="flex flex-shrink-0 items-end justify-between gap-3">
        <div className="min-w-0">
          <h1
            className="font-display font-[800] uppercase leading-[0.85] tracking-[-0.02em]"
            style={{ fontSize: "clamp(24px, 2.6vw, 46px)" }}
          >
            {headline}
          </h1>
          {signal.size_usd && (
            <div className="mono mt-1 text-[12px] font-[700] tabular-nums text-ink">
              ≈ {fmtUsd(signal.size_usd)} <span className="font-[400] text-faint">notional</span>
            </div>
          )}
        </div>
        <div className="flex-shrink-0 text-right">
          <div
            className="font-display font-[800] tabular-nums leading-[0.8]"
            style={{ fontSize: "clamp(22px, 2.3vw, 40px)", color: hot ? "var(--color-accent)" : "var(--color-ink)" }}
          >
            {mag.toFixed(2)}
          </div>
          <div className="mono text-[9px] uppercase tracking-[0.1em] text-faint">
            {scoreKind} · onchain {signal.score.toLocaleString("en-US")}
          </div>
        </div>
      </div>

      {swap ? (
        <div className="mt-2.5 flex-shrink-0 border-t border-line pt-2">
          <SwapAction swap={swap} swapHref={swapHref} />
        </div>
      ) : isProtocol && detail?.payload ? (
        <div className="mt-2.5 flex-shrink-0 border-t border-line pt-2">
          <ProtocolDetail type={signal.type} payload={detail.payload} sizeUsd={signal.size_usd} />
        </div>
      ) : null}

      {/* full analyst note, scrolls if long */}
      <div className="ocr-scroll mt-2 min-h-0 flex-1 overflow-y-auto pr-1">
        {noteText && <p className="text-[12px] leading-snug text-muted">{noteText}</p>}
      </div>

      <div className="mono mt-1.5 flex flex-shrink-0 flex-wrap items-center gap-x-4 gap-y-1 border-t border-line pt-2 text-[9px] uppercase tracking-[0.08em] text-muted">
        {signal.attest_url && <Lnk label="attest" href={signal.attest_url} />}
        {signal.tx_url && <Lnk label={txLabel} href={signal.tx_url} />}
        {signal.actor_url && <Lnk label="wallet" href={signal.actor_url} />}
        {signal.action_url && (
          <ExtLink href={signal.action_url} className={LINK} title={signal.action_label ?? undefined}>
            {actionLabel}↗
          </ExtLink>
        )}
        <ExtLink href={signal.attest_url} className={`${BTN} ml-auto`} title="Open the on-chain attestation on MantleScan">
          verify ↗
        </ExtLink>
      </div>
    </motion.div>
  );
}
