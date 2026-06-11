// Types mirror the JSON API contract.
// A missing field is requested from the backend, never invented here.

export type SignalTypeId = 1 | 2 | 3 | 4 | 5 | 6 | 7;
export type SignalTypeName =
  | "flow_anomaly"
  | "whale"
  | "smart_money"
  | "lst_flow"
  | "liquidation"
  | "big_borrow"
  | "depeg";
export type Grade = "A" | "B" | "C" | "D" | "F";
export type Metric = "swap_count" | "vol0" | "vol1" | "netflow0" | "netflow1";

export interface Outcome {
  grade: number; // 0-100
  letter: Grade;
  status: string;
  attest_tx: string | null;
  attest_url: string | null;
}

export interface Signal {
  id: number;
  created_at: string; // RFC3339 UTC
  type: SignalTypeId;
  type_name: SignalTypeName;
  pool: string;
  pool_label: string;
  metric: Metric | string;
  score: number; // on-chain int = round(min(|z|, 655.35) * 100)
  zscore: number; // raw; composite 0-100 for type 3
  actor: string | null; // set for type 2 (whale wallet) + 3/4/5/6; null for type 1 + 7
  size_usd: string | null; // display-only approx USD notional (stable side); null for flow/lst/depeg
  note: string | null; // null until enriched
  attest_tx: string | null;
  attest_url: string | null;
  status: string;
  outcome: Outcome | null;
  bucket_ts: string | null;
  // Present when sourced directly from chain (viem), absent from the JSON API.
  block?: number;
  signal_hash?: string;
  // API enrichment: links + the triggering-swap tx.
  pool_url?: string;
  actor_url?: string | null;
  tx_hash?: string | null;
  tx_url?: string | null;
  // Canonical "go interact" link + label, e.g. "Borrow on Aave" / "Stake on mETH
  // Protocol". Protocol signals (4-6); null for DEX types + depeg.
  action_url?: string | null;
  action_label?: string | null;
}

export interface TokenRef {
  address: string;
  symbol: string;
  decimals: number;
  logo_url: string | null;
}

export interface SwapBody {
  dex: string;
  token_in: TokenRef; // the token the actor sent
  token_out: TokenRef; // the token the actor received
  amount_in_raw: string; // raw units (format with token_in.decimals)
  amount_out_raw: string;
  tx_url: string | null;
}

export interface SignalDetail extends Signal {
  payload: Record<string, unknown>; // detector JSONB
  window_buckets: number;
  swap: SwapBody | null; // decoded triggering swap (type 2/3; null otherwise)
}

export interface SignalsPage {
  items: Signal[];
  total: number;
  limit: number;
  offset: number;
}

export interface GradeDistribution {
  A: number;
  B: number;
  C: number;
  D: number;
  F: number;
}

export interface ByType {
  flow: number;
  whale: number;
  smart_money: number;
  lst_flow?: number;
  liquidation?: number;
  big_borrow?: number;
  depeg?: number;
}

export interface Stats {
  total_signals: number;
  by_type: ByType;
  signal_attestations: number;
  outcome_attestations: number;
  outcomes_total: number;
  grade_distribution: GradeDistribution;
  avg_grade: number;
  last_24h: {
    signals: number;
    flow: number;
    whale: number;
    smart_money: number;
    lst_flow?: number;
    liquidation?: number;
    big_borrow?: number;
    depeg?: number;
    top_actor: string | null;
    // 24h outcome breakdown: present once enough signals are graded in-window.
    grade_distribution?: GradeDistribution;
    avg_grade?: number;
    outcomes_total?: number;
  };
  pools_tracked: number;
  first_signal_at: string | null;
  updated_at: string;
}

export interface Actor {
  actor: string;
  actor_url: string;
  total_signals: number;
  large_swaps_24h: number;
  pools_touched_24h: number;
  latest_score: number;
  best_score: number;
  grades: { count: number; avg: number };
  first_seen: string;
  last_seen: string;
}

export interface ActorDetail extends Actor {
  signals: Signal[];
  accumulation: { swaps: number; pools: number; window_hours: number };
}

export interface PoolActivity {
  // Internal: raw token units; what the detector keys off.
  swaps_24h: number;
  vol0_24h: string; // decimal string
  vol1_24h: string; // decimal string
  last_bucket_ts: string | null;
}

export interface PoolStats {
  // External: GeckoTerminal, display-only; never drives signals.
  tvl_usd: string;
  vol24h_usd: string;
  price_usd: string;
  price_change_24h: number;
  fetched_at: string;
  source: string;
}

export interface Pool {
  address: string;
  url: string;
  dex: string;
  label: string;
  token0: string;
  token1: string;
  dec0: number;
  dec1: number;
  enabled: boolean;
  token0_symbol?: string;
  token1_symbol?: string;
  token0_logo_url?: string | null;
  token1_logo_url?: string | null;
  trade_url?: string; // Matcha deep link (gasless swap, official Mantle aggregator)
  dex_url?: string; // native DEX app HOMEPAGE (fallback)
  lp_url?: string | null; // per-pool add-liquidity deep link; falls back to dex_url when absent
  activity: PoolActivity;
  stats: PoolStats | null;
  recent_signals_24h: number;
}

// Pool address (lowercase) to its two token logos. Shared UI helper map.
export type PoolLogos = Record<string, { l0?: string | null; l1?: string | null }>;

export interface PoolSeriesPoint {
  ts: string;
  swap_count: number;
  vol0: string;
  vol1: string;
}

export interface PoolDetail extends Pool {
  series: PoolSeriesPoint[];
  recent_signals: Signal[];
}

export interface SeriesPoint {
  ts: string;
  value: string; // decimal string
}

export interface Series {
  pool: string;
  metric: Metric;
  points: SeriesPoint[];
}

export interface ApiError {
  error: string;
}
