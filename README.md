# OCR (OnChain Radar)

**An autonomous on-chain analyst for Mantle whose every call is recorded on-chain, so anyone can check its work.**

On-chain "AI alpha" is everywhere and verifiable nowhere: every bot claims a hit-rate, none can prove one. OCR takes the opposite bet. It watches Mantle DeFi in real time, flags the abnormal flows, writes a short analyst note for each, and attests **every signal as a tamper-proof, timestamped record on Mantle**. After a fixed horizon it measures what actually happened and grades the call. The track record is not a screenshot you trust. It is on-chain, public, and reconstructable by anyone from the logs alone.

Built for the Mantle Turing Test Hackathon, Track 02 (AI Alpha & Data).

## See it live

- **Dashboard** (public, no wallet, no login): https://onchainradar.tech
- **Telegram channel** (live photo-card alerts): https://t.me/OCRalert
- **On-chain record**: `SignalAttestor` on Mantle mainnet `0x7bD664AdfB091E5159fE1CBfa01bFD6f2734A968`, every signal a `SignalAttested` log on [MantleScan](https://mantlescan.xyz/address/0x7bD664AdfB091E5159fE1CBfa01bFD6f2734A968)

## Why it matters

Track 02 asks for "smart-money tracking and on-chain anomaly bots." The usual answer is a Telegram bot wrapping an LLM, plus a throwaway contract deployed only to satisfy the rule. OCR makes the contract the point.

- **A track record you audit, not trust.** Each call is `keccak256`-hashed and attested on Mantle the moment it fires; the score it carries is the same integer the dashboard shows. Matured calls are graded into a reproducible report card whose hash can be committed on-chain too, so the agent's hit-rate is independently verifiable, with no access to OCR's database.
- **Detection that price cannot game.** Each pool is scored only against its own rolling baseline, in raw token units. No oracle and no USD normalization feed a single detection decision. Thin Mantle pools and new assets are first-class, and no one can manufacture a signal by moving a price. The display-only market stats also come from the chain: reserves and spot price on-chain, 24h volume from OCR's own buckets. No external price feed enters the system, and the detector never reads them.
- **The whole money map, not just stables.** Seven signal families span DEX swaps, whales, accumulating smart money, mETH / cmETH staking flows, Aave liquidations and borrows, and stablecoin depegs with a contagion map of exposed pools.

## What it detects

| # | signal | what fires it |
|--:|--------|---------------|
| 1 | `flow` | a pool's bucketed activity departs from its own MAD baseline |
| 2 | `whale` | a single swap, large relative to the pool's own median |
| 3 | `smart_money` | one wallet (resolved tx.origin) accumulating across pools, net-directional |
| 4 | `lst_flow` | a large mETH / cmETH transfer: mint (staked in), burn (unstaked), or wallet-to-wallet move |
| 5 | `liquidation` | an Aave V3 liquidation on a tracked reserve |
| 6 | `big_borrow` | a large Aave V3 borrow against a tracked reserve |
| 7 | `depeg` | a stablecoin trading off its $1 peg, with the exposed pools mapped |

Families 1 to 3 use a modified z-score (median absolute deviation) over a rolling per-pool baseline. Families 4 to 7 are sparse on-chain events gated by absolute, own-data thresholds. Any USD figure shown is derived from OCR's own decoded amounts, never from an external price feed.

## How it works

```
Mantle RPC -> collect -> Postgres -> aggregate -> detect -> enrich -> attest -> deliver
                                                     |                             |
                                                     +-----------> outcome --------+
```

One Go binary (`ocr`) and Postgres. No Redis, no queues, no other infrastructure.

- **collect** polls `eth_getLogs` for the registered pools and protocols, writes `raw_logs`, and resumes from a per-chain cursor after any restart.
- **aggregate** rolls logs into 5-minute per-pool buckets: swap count, gross volume, and net flow per token, in raw units.
- **detect** scores each pool against its own baseline and emits the signals above.
- **enrich** runs off the hot path: one structured LLM call per signal writes a short analyst note. LLM downtime never blocks detection or attestation.
- **attest** hashes the canonical signal JSON and sends the on-chain attestation, storing the tx hash on the signal row.
- **deliver** posts each signal to Telegram as a branded photo-card with a plain-language caption (key numbers in bold, never a raw z-score) and a row of inline links (on-chain proof, pool, wallet, dashboard); the analyst note is edited into the caption once enrich completes.
- **outcome** grades each matured call into a reproducible, hashable report card, optionally committed on-chain.

## On-chain attestation

Two events-only contracts (Foundry, under `contracts/`), each gated to the agent key by an owner-rotatable `onlyAgent` modifier. The agent wallet holds gas only.

- **`SignalAttestor`** emits `SignalAttested(signalHash, subject, signalType, score, ts)` once per signal. It stores no signal state, so the full history is the log stream. Scope log queries to the deployed address.
- **`OutcomeAttestor`** emits `OutcomeRecorded` per matured outcome, putting the graded hit-rate itself on-chain.

```
SignalAttestor   0x7bD664AdfB091E5159fE1CBfa01bFD6f2734A968
OutcomeAttestor  0x9161c2A88b219d3FD98CC42D868197824e679a5C
```

Deploy with the Foundry scripts in `contracts/script/` (`Deploy.s.sol`, `DeployOutcome.s.sol`); set the addresses via `ATTESTOR_ADDRESS` and `OUTCOME_ATTESTOR_ADDRESS`.

## Run it

```bash
cp .env.example .env     # DATABASE_URL, MANTLE_RPC_URL, AGENT_PRIVATE_KEY, ATTESTOR_ADDRESS,
                         # plus optional TG_* and LLM_* (every optional piece degrades gracefully)
go build ./...

for f in migrations/*.sql; do psql "$DATABASE_URL" -f "$f"; done   # apply migrations in order

./ocr run                # collect + aggregate + detect + outcome + poolstats + serve
```

`.env.example` documents every setting, including the per-detector thresholds. `ocr` also exposes one-shot subcommands (`collect`, `backfill`, `aggregate`, `detect`, `outcomes`, `discover`, `attest-pending`, `serve`, and per-family scans); run `ocr` with no arguments for the list.

## Public API

`ocr serve` (default `0.0.0.0:7070`) exposes a read-only, no-auth JSON API, the same contract the dashboard consumes:

| route | returns |
|-------|---------|
| `GET /api/stats` | headline counts, trailing-24h activity, grade distribution |
| `GET /api/signals` | paged signals with attestation tx and analyst note (`?graded=true` for matured calls) |
| `GET /api/signals/{id}` | one signal, including the decoded triggering swap |
| `GET /api/actors` / `/{addr}` | smart-money leaderboard and per-actor footprint |
| `GET /api/pools` / `/{addr}` | pool registry, 24h on-chain activity, display-only on-chain market stats |
| `GET /api/series` | a pool metric as a time series |
| `GET /api/live` | Server-Sent Events stream of new signals |

## Repository layout

```
cmd/ocr/        single binary, subcommands
internal/
  chain/        ethclient dial + reorg-aware polling collector
  store/        pgx pool, models, queries
  aggregate/    raw_logs -> time buckets
  detect/       MAD z-score engine + the seven detector families
  attest/       signal hashing + attestation transactions
  enrich/       LLM analyst note (provider interface)
  deliver/      Telegram client + dispatcher + photo-card renderer
  outcome/      matured-signal grading + report-card hashing
  discover/     on-chain-verified pool discovery
  poolstats/    display-only on-chain market snapshot
  api/          read-only JSON API + SSE
  config/       env + YAML loader
contracts/      Foundry: SignalAttestor + OutcomeAttestor, tests, deploy scripts
migrations/     ordered SQL schema
web/            Next.js dashboard
```

## Roadmap

- Smart-money wallet labels on the actor leaderboard (the provider interface is already in place, off by default).
- Outcome-weighted reputation: rank actors and signal families by their proven, on-chain hit-rate.
- More Mantle protocols behind the same attestation contract: a new signal family is a new `signalType`, no redeploy.

## License

MIT. See `LICENSE`.
