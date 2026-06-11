"use client";

import { AlertTriangle, RefreshCw } from "lucide-react";
import { BTN } from "./actions";

export function Skeleton({ className = "" }: { className?: string }) {
  return <div className={`animate-pulse bg-surface ${className}`} />;
}

export function EmptyState({ title, sub }: { title: string; sub?: string }) {
  return (
    <div className="flex flex-col items-center justify-center gap-3 px-8 py-16 text-center">
      <div className="mono text-[11px] uppercase tracking-[0.16em] text-faint">{title}</div>
      {sub && <div className="mono text-[12px] text-muted">{sub}</div>}
    </div>
  );
}

export function ErrorState({
  message,
  onRetry,
}: {
  message: string;
  onRetry?: () => void;
}) {
  return (
    <div
      className="mono flex items-center justify-between gap-4 border border-accent px-3 py-2 text-accent"
      role="alert"
    >
      <span className="flex items-center gap-2 text-[11px] uppercase tracking-[0.08em]">
        <AlertTriangle size={13} /> {message}
      </span>
      {onRetry && (
        <button data-cursor onClick={onRetry} className={BTN}>
          <RefreshCw size={12} /> retry
        </button>
      )}
    </div>
  );
}
