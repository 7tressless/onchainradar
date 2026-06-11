// Typed fixtures mirroring the API contract, so the dashboard renders and the live
// feed streams without a running backend (dev only; toggle via NEXT_PUBLIC_USE_FIXTURES).
// Every shape matches lib/types; data uses the real pools (config/pools.yaml), real
// metrics, raw-unit volumes (no USD), and the on-chain score rule (z*100, cap 65535).
import type { Actor, Grade, Pool, Signal, SignalTypeId, Stats } from "./types";
import type { SseHandlers } from "./sse";

export const USE_FIXTURES = process.env.NEXT_PUBLIC_USE_FIXTURES === "1";

const SCAN = "https://mantlescan.xyz";
const USDe = "0x5d3a1ff2b6bab83b63cd9ad0787074081a52ef34";
const WMNT = "0x78c1b0c915c4faa5fffa6cabf0219da63d7f4cb8";
const USDC = "0x09bc4e0d864854c6afb6eb9a9cdf58ac190d0df9";
const USDT = "0x201eba5cc46d216ce6dc03f6a759e8e766e956ae";

const iso = (minsAgo: number) => new Date(Date.now() - minsAgo * 60_000).toISOString();
const txUrl = (h: string) => `${SCAN}/tx/${h}`;
const hash = (seed: number, n = 8) =>
  "0x" + Math.abs(Math.sin(seed) * 1e16).toString(16).slice(0, n);

export function fxPools(): Pool[] {
  const mk = (
    address: string,
    dex: string,
    label: string,
    t0: string,
    t1: string,
    d0: number,
    d1: number,
    a: Pool["activity"],
    s: Pool["stats"],
    rs: number,
  ): Pool => ({
    address,
    url: `${SCAN}/address/${address}`,
    dex,
    label,
    token0: t0,
    token1: t1,
    dec0: d0,
    dec1: d1,
    enabled: true,
    activity: a,
    stats: s,
    recent_signals_24h: rs,
  });
  return [
    mk("0xeafc4d6d4c3391cd4fc10c85d2f5f972d58c0dd5", "agni", "USDe/WMNT", USDe, WMNT, 18, 18,
      { swaps_24h: 1840, vol0_24h: "4821339.55", vol1_24h: "4790112.81", last_bucket_ts: iso(2) },
      { tvl_usd: "3210000", vol24h_usd: "5120000", price_usd: "0.9994", price_change_24h: -0.04, fetched_at: iso(3), source: "geckoterminal" }, 7),
    mk("0x5d54d430d1fd9425976147318e6080479bffc16d", "merchant_moe", "USDe/WMNT", USDe, WMNT, 18, 18,
      { swaps_24h: 1233, vol0_24h: "3190874.10", vol1_24h: "3175001.92", last_bucket_ts: iso(4) },
      { tvl_usd: "3190000", vol24h_usd: "3380000", price_usd: "0.9996", price_change_24h: 0.02, fetched_at: iso(3), source: "geckoterminal" }, 5),
    mk("0x7e78b65d0525339df5f4aa22b82d9e97584da8fc", "merchant_moe", "USDe/USDC", USDe, USDC, 18, 6,
      { swaps_24h: 902, vol0_24h: "2140550.00", vol1_24h: "2139880.40", last_bucket_ts: iso(7) },
      { tvl_usd: "2140000", vol24h_usd: "2210000", price_usd: "1.0001", price_change_24h: 0.01, fetched_at: iso(3), source: "geckoterminal" }, 3),
    mk("0xbcf99c834e65e8a58090e20edc058279317865bd", "agni", "USDC/USDe", USDC, USDe, 6, 18,
      { swaps_24h: 744, vol0_24h: "1701220.18", vol1_24h: "1700640.05", last_bucket_ts: iso(11) },
      { tvl_usd: "1700000", vol24h_usd: "1620000", price_usd: "0.9999", price_change_24h: -0.01, fetched_at: iso(3), source: "geckoterminal" }, 4),
    mk("0x36a7aff497eef6a9cd7d0e7bc243793fcb3e57e2", "agni", "USDT/USDe", USDT, USDe, 6, 18,
      { swaps_24h: 511, vol0_24h: "1532980.71", vol1_24h: "1531004.66", last_bucket_ts: iso(19) },
      { tvl_usd: "1530000", vol24h_usd: "1410000", price_usd: "1.0000", price_change_24h: 0.0, fetched_at: iso(3), source: "geckoterminal" }, 2),
    mk("0x365722f12ceb2063286a268b03c654df81b7c00f", "merchant_moe", "WMNT/USDT", WMNT, USDT, 18, 6,
      { swaps_24h: 1290, vol0_24h: "1061450.33", vol1_24h: "1060980.12", last_bucket_ts: iso(5) },
      null, 6),
  ];
}

const ACTORS: { addr: string; latest: number; best: number; large: number; pools: number; sig: number; gc: number; ga: number }[] = [
  { addr: "0x8d58a3f1c0b2e7a9d4f6c1b8e2a05d7f3c9b6e10", latest: 94, best: 97, large: 11, pools: 4, sig: 18, gc: 9, ga: 84 },
  { addr: "0x2c7b9e41a0d3f85c6b1e2a9d7f04c3b8e6a1d052", latest: 88, best: 92, large: 8, pools: 3, sig: 12, gc: 6, ga: 79 },
  { addr: "0x5af1c8e20b9d34a7f6c1e8b2a0d9f7c43b1e60a8", latest: 76, best: 90, large: 6, pools: 3, sig: 9, gc: 5, ga: 71 },
  { addr: "0x91d4a2ff60bb18c3e7a0d9f5c2b8e41a6d037c2e", latest: 71, best: 83, large: 5, pools: 2, sig: 7, gc: 4, ga: 68 },
  { addr: "0xa07e55d1c3f02b9e8a4d6f1c0b7e2a93d5f81c64", latest: 64, best: 77, large: 4, pools: 2, sig: 5, gc: 3, ga: 62 },
];

export function fxActors(): Actor[] {
  return ACTORS.map((a) => ({
    actor: a.addr,
    actor_url: `${SCAN}/address/${a.addr}`,
    total_signals: a.sig,
    large_swaps_24h: a.large,
    pools_touched_24h: a.pools,
    latest_score: a.latest,
    best_score: a.best,
    grades: { count: a.gc, avg: a.ga },
    first_seen: iso(60 * 24 * 6),
    last_seen: iso(3),
  }));
}

// (poolLabel, dex, addr, type, metric, z, note, actorIdx?, gradeLetter?)
type Tpl = [string, string, string, SignalTypeId, string, number, string, number?, Grade?];
const POOL_ADDR: Record<string, string> = {
  "USDe/WMNT·agni": "0xeafc4d6d4c3391cd4fc10c85d2f5f972d58c0dd5",
  "USDe/WMNT·moe": "0x5d54d430d1fd9425976147318e6080479bffc16d",
  "USDe/USDC·moe": "0x7e78b65d0525339df5f4aa22b82d9e97584da8fc",
  "USDC/USDe·agni": "0xbcf99c834e65e8a58090e20edc058279317865bd",
  "USDT/USDe·agni": "0x36a7aff497eef6a9cd7d0e7bc243793fcb3e57e2",
  "WMNT/USDT·moe": "0x365722f12ceb2063286a268b03c654df81b7c00f",
};

const SEED: Tpl[] = [
  ["USDe/WMNT", "agni", "USDe/WMNT·agni", 1, "vol0", 1031.4, "USDe/WMNT volume ~297× the pool's 48h median. Sharp one-sided USDe inflow.", undefined, "A"],
  ["USDe/WMNT", "moe", "USDe/WMNT·moe", 1, "swap_count", 247.8, "Swap count ~27× baseline. Burst of many small swaps.", undefined, "B"],
  ["USDC/USDe", "agni", "USDC/USDe·agni", 3, "netflow0", 94.2, "EOA net-accumulating USDe across 3 pools, churn-filtered.", 0, "A"],
  ["WMNT/USDT", "moe", "WMNT/USDT·moe", 2, "vol0", 41.2, "Single swap ~41× the pool's median trade size.", undefined, "C"],
  ["USDe/USDC", "moe", "USDe/USDC·moe", 1, "vol1", 31.2, "USDC volume ~8× baseline, balanced two-way flow.", undefined, "B"],
  ["USDe/WMNT", "agni", "USDe/WMNT·agni", 3, "netflow0", 88.7, "Repeated large USDe buys across pools; accumulation.", 1, "B"],
  ["USDT/USDe", "agni", "USDT/USDe·agni", 2, "vol0", 33.5, "Single swap ~33× median trade, large taker.", undefined, "C"],
  ["USDC/USDe", "agni", "USDC/USDe·agni", 1, "netflow1", 18.9, "One-sided USDe accumulation ~6× normal.", undefined, "D"],
  ["WMNT/USDT", "moe", "WMNT/USDT·moe", 1, "swap_count", 12.4, "Swap count ~3× baseline, no price impact.", undefined, "C"],
  ["USDe/WMNT", "moe", "USDe/WMNT·moe", 3, "netflow0", 71.3, "Smaller cross-pool accumulation, churn-filtered.", 2, "B"],
  ["USDT/USDe", "agni", "USDT/USDe·agni", 1, "vol0", 9.6, "USDT volume ~3× baseline, mild widening.", undefined, "F"],
  ["USDe/USDC", "moe", "USDe/USDC·moe", 2, "vol1", 27.8, "Single swap ~28× median trade.", undefined, "C"],
];

function mkSig(tpl: Tpl, id: number, minsAgo: number, pending = false): Signal {
  const [label, dex, key, type, metric, z, note, actorIdx, grade] = tpl;
  const score = Math.round(Math.min(Math.abs(z), 655.35) * 100);
  const typeName = type === 1 ? "flow_anomaly" : type === 2 ? "whale" : "smart_money";
  const actor = actorIdx !== undefined ? ACTORS[actorIdx].addr : null;
  const at = hash(id + 1, 12) + "a968";
  const gradeNum: Record<Grade, number> = { A: 88, B: 79, C: 66, D: 54, F: 38 };
  const outcome =
    !pending && grade
      ? {
          grade: gradeNum[grade],
          letter: grade,
          status: "graded",
          attest_tx: hash(id + 500, 12) + "f5c",
          attest_url: txUrl(hash(id + 500, 12) + "f5c"),
        }
      : null;
  return {
    id,
    created_at: iso(minsAgo),
    type,
    type_name: typeName,
    pool: POOL_ADDR[key],
    pool_label: `${label} · ${dex.toUpperCase()}`,
    metric,
    score,
    zscore: z,
    actor,
    size_usd: null,
    note,
    attest_tx: pending ? null : at,
    attest_url: pending ? null : txUrl(at),
    status: pending ? "submitted" : "attested",
    outcome,
    bucket_ts: iso(minsAgo),
  };
}

const BASE_ID = 5280;

export function fxRecentSignals(n = 12): Signal[] {
  const out: Signal[] = [];
  for (let i = 0; i < Math.min(n, SEED.length); i++) {
    out.push(mkSig(SEED[i], BASE_ID - i, 2 + i * 7));
  }
  return out;
}

export function fxStats(): Stats {
  return {
    total_signals: 528,
    by_type: { flow: 361, whale: 112, smart_money: 55 },
    signal_attestations: 528,
    outcome_attestations: 419,
    outcomes_total: 419,
    grade_distribution: { A: 71, B: 138, C: 121, D: 58, F: 31 },
    avg_grade: 68.4,
    last_24h: { signals: 47, flow: 31, whale: 11, smart_money: 5, top_actor: ACTORS[0].addr },
    pools_tracked: 6,
    first_signal_at: iso(60 * 24 * 5),
    updated_at: iso(0),
  };
}

// Simulated SSE for fixtures mode; mirrors subscribeLive's interface so the live
// hook is source-agnostic. Replays recent signals, then streams a fresh one every
// ~3.5s (with an occasional high-score smart_money to exercise the flare).
let liveSeq = BASE_ID + 1;
const LIVE_POOL: Tpl[] = [
  ["USDe/WMNT", "agni", "USDe/WMNT·agni", 1, "vol0", 22.4, "USDe volume ~6× baseline, just attested.", undefined],
  ["USDC/USDe", "agni", "USDC/USDe·agni", 3, "netflow0", 96.1, "Strong cross-pool USDe accumulation, churn-filtered.", 0, "A"],
  ["WMNT/USDT", "moe", "WMNT/USDT·moe", 2, "vol0", 38.9, "Single swap ~39× median trade.", undefined],
  ["USDe/USDC", "moe", "USDe/USDC·moe", 1, "swap_count", 14.7, "Swap count ~4× baseline.", undefined],
  ["USDe/WMNT", "moe", "USDe/WMNT·moe", 3, "netflow0", 81.5, "EOA adding across pools; accumulation.", 1],
  ["USDT/USDe", "agni", "USDT/USDe·agni", 1, "vol0", 11.2, "USDT volume ~3× baseline.", undefined],
];
let liveIdx = 0;

function nextLiveSignal(): Signal {
  const tpl = LIVE_POOL[liveIdx % LIVE_POOL.length];
  liveIdx += 1;
  return mkSig(tpl, liveSeq++, 0, true);
}

export function subscribeLiveFixtures(h: SseHandlers): () => void {
  let closed = false;
  h.onStatus?.("connecting");
  const replay = fxRecentSignals(12).reverse(); // oldest-first
  const t0 = setTimeout(() => {
    if (closed) return;
    h.onStatus?.("live");
    replay.forEach((s) => h.onSignal(s));
  }, 300);
  const iv = setInterval(() => {
    if (closed) return;
    h.onSignal(nextLiveSignal());
  }, 3500);
  return () => {
    closed = true;
    clearTimeout(t0);
    clearInterval(iv);
  };
}
