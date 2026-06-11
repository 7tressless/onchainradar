// Server-side chain reader (viem). Reads SignalAttested events from the verified
// contract on Mantle mainnet: tx hashes, pools, scores, timestamps. Imported only
// by route handlers (app/api/chain/*), never the client.
import "server-only"; // build-time guarantee: a client import of this module fails the build
import { createPublicClient, http, parseAbiItem } from "viem";
import { shortAddr } from "./format";
import type { Signal, SignalTypeId } from "./types";

// Runtime backstop for the server-only import above: fail fast if this module is
// ever evaluated in a browser bundle (which would ship viem + the RPC config to the client).
if (typeof window !== "undefined") {
  throw new Error("lib/chain is server-only; import it from route handlers only");
}

const ZERO_ADDR = "0x0000000000000000000000000000000000000000";

const RPC = process.env.MANTLE_RPC_URL ?? "https://rpc.mantle.xyz";
const SIGNAL_ATTESTOR = "0x7bD664AdfB091E5159fE1CBfa01bFD6f2734A968";
const SCAN = "https://mantlescan.xyz";

const client = createPublicClient({ transport: http(RPC) });

// SignalAttested ABI (signalType is indexed). Matches the verified contract on MantleScan.
const SIGNAL_EVENT = parseAbiItem(
  "event SignalAttested(bytes32 indexed signalHash, address indexed subject, uint8 indexed signalType, uint16 score, uint64 ts)",
);

const TYPE_NAMES: Record<number, Signal["type_name"]> = {
  1: "flow_anomaly",
  2: "whale",
  3: "smart_money",
  4: "lst_flow",
  5: "liquidation",
  6: "big_borrow",
  7: "depeg",
};

// Curated pool registry (config/pools.yaml), lowercase keys for matching.
const REGISTRY: Record<string, { label: string; dex: string }> = {
  "0xeafc4d6d4c3391cd4fc10c85d2f5f972d58c0dd5": { label: "USDe/WMNT", dex: "AGNI" },
  "0x5d54d430d1fd9425976147318e6080479bffc16d": { label: "USDe/WMNT", dex: "MOE" },
  "0x7e78b65d0525339df5f4aa22b82d9e97584da8fc": { label: "USDe/USDC", dex: "MOE" },
  "0xbcf99c834e65e8a58090e20edc058279317865bd": { label: "USDC/USDe", dex: "AGNI" },
  "0x36a7aff497eef6a9cd7d0e7bc243793fcb3e57e2": { label: "USDT/USDe", dex: "AGNI" },
  "0x365722f12ceb2063286a268b03c654df81b7c00f": { label: "WMNT/USDT", dex: "MOE" },
};

// Short in-process cache: the client polls every 2s per viewer; without it each request
// would hit the public RPC.
let headCache: { at: number; block: number } | null = null;
const HEAD_TTL = 1500;

export async function getHeadBlock(): Promise<number> {
  if (headCache && Date.now() - headCache.at < HEAD_TTL) return headCache.block;
  const block = Number(await client.getBlockNumber());
  headCache = { at: Date.now(), block };
  return block;
}

type Cache = { at: number; data: Signal[] };
let cache: Cache | null = null;
const TTL = 4000;
// Single-flight: collapse concurrent cold requests so they don't each run 16 getLogs
// calls against the public RPC (which the provider would rate-limit).
let inflight: Promise<Signal[]> | null = null;

// Chunked getLogs back from head (public RPC range-friendly) until we have enough.
export async function fetchRecentSignals(limit = 60): Promise<Signal[]> {
  if (cache && Date.now() - cache.at < TTL) return cache.data;
  if (inflight) return inflight;
  inflight = doFetchRecentSignals(limit).finally(() => {
    inflight = null;
  });
  return inflight;
}

async function doFetchRecentSignals(limit: number): Promise<Signal[]> {
  const head = BigInt(await getHeadBlock()); // reuses the short head cache
  const SPAN = 9000n;
  let to = head;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const logs: any[] = [];
  for (let i = 0; i < 16; i++) {
    const from = to - SPAN > 0n ? to - SPAN : 0n;
    try {
      const part = await client.getLogs({
        address: SIGNAL_ATTESTOR,
        event: SIGNAL_EVENT,
        fromBlock: from,
        toBlock: to,
      });
      logs.push(...part);
    } catch (e) {
      // Log and stop rather than cache a partial range as the full history.
      console.error(`chain getLogs chunk failed (blocks ${from}-${to}):`, e);
      break;
    }
    to = from - 1n;
    if (logs.length >= limit || from === 0n) break;
  }

  const signals = logs
    .map(mapLog)
    .filter((s) => s.pool !== ZERO_ADDR) // drop zero-address noise attestations
    .sort((a, b) => b.id - a.id)
    .slice(0, limit);
  cache = { at: Date.now(), data: signals };
  return signals;
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
function mapLog(log: any): Signal {
  const subject = String(log.args.subject).toLowerCase();
  const type = Number(log.args.signalType) as SignalTypeId;
  const score = Number(log.args.score);
  const tsSec = Number(log.args.ts);
  const reg = REGISTRY[subject];
  const tx = String(log.transactionHash);
  const block = Number(log.blockNumber);
  // logIndex is block-wide (all contracts) and routinely exceeds 99 on a busy chain,
  // so a 100k stride keeps ids collision-free and well inside Number.MAX_SAFE_INTEGER.
  const id = block * 100_000 + Number(log.logIndex ?? 0);
  const isoTs = new Date(tsSec * 1000).toISOString();
  return {
    id,
    created_at: isoTs,
    type,
    type_name: TYPE_NAMES[type] ?? "flow_anomaly",
    pool: subject,
    pool_label: reg ? `${reg.label} · ${reg.dex}` : shortAddr(subject),
    metric: "",
    score,
    zscore: score / 100,
    actor: null,
    size_usd: null,
    note: null,
    attest_tx: tx,
    attest_url: `${SCAN}/tx/${tx}`,
    status: "attested",
    outcome: null,
    bucket_ts: isoTs,
    block,
    signal_hash: String(log.args.signalHash),
  };
}
