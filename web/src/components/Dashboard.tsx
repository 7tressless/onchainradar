"use client";

import { useEffect, useMemo, useState, type ReactNode } from "react";
import { BlendCursor } from "./BlendCursor";
import { Chrome } from "./Chrome";
import { Composition } from "./Composition";
import { GradeDonut } from "./GradeDonut";
import { Hero } from "./Hero";
import { Leaderboard } from "./Leaderboard";
import { LiveFeed } from "./LiveFeed";
import { Panel } from "./bp";
import { PoolCards } from "./PoolCards";
import { RiskBanner } from "./RiskBanner";
import { StatCards } from "./StatCards";
import { Tape } from "./Tape";
import { TrackRecord } from "./TrackRecord";
import { EmptyState, ErrorState, Skeleton } from "./states";
import { useActors, useChainSignals, useLiveFeed, usePools, useSignals, useStats } from "@/lib/hooks";
import type { SseStatus } from "@/lib/sse";
import type { PoolLogos, Signal } from "@/lib/types";

// Live-status chip for the feed panel header.
function liveChip(status: SseStatus): ReactNode {
  if (status === "live")
    return (
      <span className="flex items-center gap-1.5 text-accent">
        <span className="h-[5px] w-[5px] rounded-full bg-accent" style={{ animation: "ocr-pulse 1.9s ease-in-out infinite" }} />
        live · click to inspect
      </span>
    );
  if (status === "reconnecting") return "reconnecting…";
  if (status === "offline") return "offline";
  return "connecting…";
}

// One place for the loading / error / empty fork of a data panel body.
function PanelBody({
  isError,
  isLoaded,
  isEmpty,
  emptyTitle,
  emptySub,
  onRetry,
  children,
}: {
  isError: boolean;
  isLoaded: boolean;
  isEmpty: boolean;
  emptyTitle: string;
  emptySub?: string;
  onRetry: () => void;
  children: ReactNode;
}) {
  // Stale data beats an error takeover: only show the error when there's no data at all.
  if (isError && !isLoaded)
    return <div className="p-3"><ErrorState message="data unavailable" onRetry={onRetry} /></div>;
  if (!isLoaded)
    return (
      <div className="flex flex-col gap-1.5 p-3">
        {Array.from({ length: 7 }).map((_, i) => (
          <Skeleton key={i} className="h-[26px]" />
        ))}
      </div>
    );
  if (isEmpty) return <EmptyState title={emptyTitle} sub={emptySub} />;
  return <>{children}</>;
}

function LoadingGrid() {
  return (
    <main className="relative z-10 flex min-h-0 flex-1 flex-col gap-2.5 px-6 py-2.5">
      <div className="grid h-[220px] flex-shrink-0 grid-cols-[1.15fr_1fr_0.95fr] gap-2.5">
        <Skeleton className="h-full" />
        <Skeleton className="h-full" />
        <Skeleton className="h-full" />
      </div>
      <Skeleton className="h-[84px] flex-shrink-0" />
      <div className="grid min-h-0 flex-1 grid-cols-3 gap-2.5">
        <Skeleton />
        <Skeleton />
        <Skeleton />
      </div>
      <Skeleton className="h-[150px] flex-shrink-0" />
    </main>
  );
}

export function Dashboard() {
  const stats = useStats();
  const live = useLiveFeed(); // real SSE stream (the "live" feed)
  const sig = useSignals(100); // polled list (feed history + hero fallback)
  const graded = useSignals(50, true); // graded-only, server-filtered (?graded=true) for the track record
  const actors = useActors(50);
  const pools = usePools();
  const { signals: chainSignals, newIds: chainNewIds } = useChainSignals();

  const liveSignals = live.signals;
  const recordSignals = useMemo(() => sig.data?.items ?? [], [sig.data]);
  // The live feed = recent history (reliable polled list) ∪ the SSE live overlay, so it
  // never empties when the SSE replay buffer is thin (e.g. right after a backend redeploy).
  const feedSignals = useMemo(() => {
    const seen = new Set<number>();
    const out: Signal[] = [];
    for (const s of [...liveSignals, ...recordSignals]) {
      if (!seen.has(s.id)) {
        seen.add(s.id);
        out.push(s);
      }
    }
    // Cap the rendered rows; the merged list is unbounded over a long session.
    return out.sort((a, b) => b.id - a.id).slice(0, 120);
  }, [liveSignals, recordSignals]);
  const sortedActors = useMemo(
    () => [...(actors.data?.items ?? [])].sort((a, b) => (b.best_score ?? 0) - (a.best_score ?? 0)),
    [actors.data],
  );
  // Cross-protocol footprint: which signal types each wallet has touched recently,
  // derived from the already-loaded signals (no per-actor fetch).
  const actorTypes = useMemo(() => {
    const sets: Record<string, Set<number>> = {};
    for (const s of feedSignals) {
      if (!s.actor) continue;
      const k = s.actor.toLowerCase();
      if (!sets[k]) sets[k] = new Set();
      sets[k].add(s.type);
    }
    const out: Record<string, number[]> = {};
    for (const k in sets) out[k] = [...sets[k]].sort((x, y) => x - y);
    return out;
  }, [feedSignals]);
  // Active stablecoin depeg (rare) → the global risk banner.
  const depeg = useMemo(() => feedSignals.find((s) => s.type === 7), [feedSignals]);
  // Track record = the graded, on-chain calls. Prefer the server-filtered ?graded=true
  // list; if it's empty (backend without the param yet) fall back to filtering the full
  // polled list by outcome, so the panel never goes blank.
  const gradedSignals = useMemo(() => {
    const server = (graded.data?.items ?? []).filter((s) => s.outcome);
    return server.length ? server : recordSignals.filter((s) => s.outcome);
  }, [graded.data, recordSignals]);
  const poolLogos = useMemo(() => {
    const m: PoolLogos = {};
    for (const p of pools.data?.items ?? []) {
      m[p.address.toLowerCase()] = { l0: p.token0_logo_url, l1: p.token1_logo_url };
    }
    return m;
  }, [pools.data]);
  // address → pre-loaded swap UI (Matcha pair, or the native DEX) for the hero CTA.
  const poolHref = useMemo(() => {
    const m: Record<string, string> = {};
    for (const p of pools.data?.items ?? []) {
      const href = p.trade_url?.length ? p.trade_url : p.dex_url?.length ? p.dex_url : "";
      if (href) m[p.address.toLowerCase()] = href;
    }
    return m;
  }, [pools.data]);

  const [selId, setSelId] = useState<number | null>(null);
  // Pin the hero to the first signal once data loads, so the analyst note isn't yanked
  // to a new signal on every poll (also avoids re-fetching detail each tick). New
  // signals still stream into the feed; click any row to inspect.
  useEffect(() => {
    if (selId === null && feedSignals.length > 0) setSelId(feedSignals[0].id);
  }, [selId, feedSignals]);
  const selected = useMemo(
    () => feedSignals.find((s) => s.id === selId) ?? feedSignals[0],
    [feedSignals, selId],
  );
  const heroSwapHref = selected ? (poolHref[(selected.pool ?? "").toLowerCase()] ?? null) : null;

  return (
    <div className="relative flex min-h-[100dvh] flex-col lg:h-screen lg:overflow-hidden">
      <div className="bp-grid" aria-hidden />
      <div className="bp-grain" aria-hidden />
      <BlendCursor />

      <div className="relative z-30 flex-shrink-0">
        <Chrome count={stats.data?.total_signals} />
      </div>

      {depeg && (
        <div className="relative z-30 flex-shrink-0">
          <RiskBanner signal={depeg} onInspect={() => setSelId(depeg.id)} />
        </div>
      )}

      {stats.isError && !stats.data ? (
        <div className="relative z-10 px-8 py-8">
          <ErrorState
            message="Agent API unreachable, retrying automatically"
            onRetry={() => {
              stats.refetch();
              sig.refetch();
            }}
          />
        </div>
      ) : !stats.data ? (
        <LoadingGrid />
      ) : (
        <main className="relative z-10 flex min-h-0 flex-1 flex-col gap-2.5 px-3 py-2.5 sm:px-6 lg:overflow-hidden">
          {/* row 1: hero / composition / grade */}
          <div className="contents lg:grid lg:h-[220px] lg:flex-shrink-0 lg:grid-cols-[1.15fr_1fr_0.95fr] lg:gap-2.5">
            <Panel className="order-4 min-h-[260px] lg:order-none lg:min-h-0" resetKey={selected?.id}>
              <Hero signal={selected} swapHref={heroSwapHref} />
            </Panel>
            <Panel title="Signal composition" className="order-1 min-h-[190px] lg:order-none lg:min-h-0" resetKey={stats.data}>
              <Composition types24={stats.data.last_24h} typesAll={stats.data.by_type} />
            </Panel>
            <Panel title="Grade distribution" right="all time" className="order-2 min-h-[190px] lg:order-none lg:min-h-0" resetKey={stats.data}>
              <GradeDonut
                grades={stats.data.grade_distribution}
                unrated={Math.max(0, stats.data.total_signals - stats.data.outcomes_total)}
              />
            </Panel>
          </div>

          {/* row 2: stat cards */}
          <div className="order-3 flex-shrink-0 lg:order-none">
            <StatCards stats={stats.data} />
          </div>

          {/* row 3: feed / record / leaderboard */}
          <div className="order-5 grid grid-cols-1 gap-2.5 lg:order-none lg:min-h-0 lg:flex-1 lg:grid-cols-3">
            <Panel title="Live signal feed" right={liveChip(live.status)} className="h-[440px] lg:h-auto lg:min-h-0" bodyClassName="ocr-scroll overflow-y-auto" resetKey={feedSignals}>
              <LiveFeed
                signals={feedSignals}
                status={live.status}
                newIds={live.newIds}
                selectedId={selected?.id}
                onSelect={setSelId}
                logos={poolLogos}
              />
            </Panel>
            <Panel title="Track record" right="graded calls · on-chain proof" className="h-[440px] lg:h-auto lg:min-h-0" bodyClassName="ocr-scroll overflow-y-auto" resetKey={gradedSignals}>
              <PanelBody
                isError={graded.isError && sig.isError}
                isLoaded={!!graded.data || !!sig.data}
                isEmpty={gradedSignals.length === 0}
                emptyTitle="No graded calls yet"
                emptySub="A signal is graded once its outcome resolves; proven calls land here."
                onRetry={() => {
                  graded.refetch();
                  sig.refetch();
                }}
              >
                <TrackRecord signals={gradedSignals} logos={poolLogos} />
              </PanelBody>
            </Panel>
            <Panel title="Smart-money leaderboard" right="by best score" className="h-[440px] lg:h-auto lg:min-h-0" bodyClassName="ocr-scroll overflow-y-auto" resetKey={sortedActors}>
              <PanelBody
                isError={actors.isError}
                isLoaded={!!actors.data}
                isEmpty={sortedActors.length === 0}
                emptyTitle="No smart-money actors yet"
                onRetry={() => actors.refetch()}
              >
                <Leaderboard actors={sortedActors} footprint={actorTypes} />
              </PanelBody>
            </Panel>
          </div>

          {/* row 4: pool overview */}
          <div className="order-6 flex-shrink-0 lg:order-none">
            <Panel title="Pool overview · 24h" right="real market · click to trade" resetKey={pools.data}>
              <PanelBody
                isError={pools.isError}
                isLoaded={!!pools.data}
                isEmpty={(pools.data?.items ?? []).length === 0}
                emptyTitle="No pools"
                onRetry={() => pools.refetch()}
              >
                <PoolCards pools={pools.data?.items ?? []} />
              </PanelBody>
            </Panel>
          </div>
        </main>
      )}

      <div className="relative z-30 flex-shrink-0">
        <Tape signals={chainSignals} newIds={chainNewIds} />
      </div>
    </div>
  );
}
