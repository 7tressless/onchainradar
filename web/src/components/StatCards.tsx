"use client";

import { memo } from "react";
import { Activity, CheckCircle2, Gauge, Layers, Wallet } from "lucide-react";
import { shortAddr } from "@/lib/format";
import type { Stats } from "@/lib/types";

function Card({
  icon,
  label,
  value,
  sub,
  className = "",
}: {
  icon: React.ReactNode;
  label: string;
  value: React.ReactNode;
  sub?: string;
  className?: string;
}) {
  return (
    <div className={`flex flex-col gap-2 bg-bg px-4 py-3 ${className}`}>
      <div className="mono flex items-center gap-2 text-[10px] uppercase tracking-[0.14em] text-faint">
        <span className="text-muted">{icon}</span>
        {label}
      </div>
      <div className="font-display text-[26px] font-[800] leading-none tabular-nums">{value}</div>
      {sub && <div className="mono text-[10px] uppercase tracking-[0.1em] text-muted">{sub}</div>}
    </div>
  );
}

export const StatCards = memo(function StatCards({ stats }: { stats: Stats }) {
  const g = stats.grade_distribution;
  const graded = g.A + g.B + g.C + g.D + g.F;
  const successRate = graded > 0 ? ((g.A + g.B) / graded) * 100 : 0;

  return (
    <div className="grid grid-cols-2 gap-px border border-line bg-line sm:grid-cols-3 lg:grid-cols-5">
      <Card icon={<Activity size={13} />} label="Signals 24h" value={stats.last_24h.signals} sub={`${stats.total_signals} all-time`} />
      <Card icon={<CheckCircle2 size={13} />} label="Success rate" value={`${successRate.toFixed(1)}%`} sub="graded A or B" />
      <Card icon={<Gauge size={13} />} label="Avg grade" value={stats.avg_grade.toFixed(1)} sub={`${graded} graded`} />
      <Card icon={<Layers size={13} />} label="Active pools" value={stats.pools_tracked} sub="monitored" />
      <Card
        icon={<Wallet size={13} />}
        label="Top actor 24h"
        className="col-span-2 items-center text-center sm:col-span-1 sm:items-start sm:text-left"
        value={
          <span className="mono text-[16px]">
            {stats.last_24h.top_actor ? shortAddr(stats.last_24h.top_actor, 6, 4) : "—"}
          </span>
        }
        sub="smart money"
      />
    </div>
  );
});
