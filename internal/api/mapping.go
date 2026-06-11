package api

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"ocr/internal/aggregate"
	"ocr/internal/attest"
	"ocr/internal/store"
)

// This file is the only place store rows become public DTOs, so the shape is auditable
// and testable without a DB or HTTP. Every nullable DB field maps to an explicit JSON
// null via a pointer, and nothing internal (tg_message_id, key material, env) is copied
// into a DTO.

// jsonRaw aliases json.RawMessage so a passed-through blob (the detector payload) is
// embedded verbatim as a nested object, not re-encoded as a string.
type jsonRaw = json.RawMessage

// marshalJSON is the single JSON encoder for every response and SSE payload, so the
// encoder choice (stdlib encoding/json) is swappable in one place.
func marshalJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}

// signalTypeName returns the stable string name for a numeric signal_type (see the
// switch below). An unknown type renders as "type_N" so a future detector is not
// silently mislabeled.
func signalTypeName(t int16) string {
	switch t {
	case store.SignalTypeFlow:
		return "flow_anomaly"
	case store.SignalTypeWhale:
		return "whale"
	case store.SignalTypeSmartMoney:
		return "smart_money"
	case store.SignalTypeLSTFlow:
		return "lst_flow"
	case store.SignalTypeLiquidation:
		return "liquidation"
	case store.SignalTypeBigBorrow:
		return "big_borrow"
	case store.SignalTypeDepeg:
		return "depeg"
	default:
		return fmt.Sprintf("type_%d", t)
	}
}

// rfc3339Ptr renders a non-zero time as an RFC3339 (UTC) string pointer, or nil
// for the zero time (so an absent timestamp serializes as JSON null).
func rfc3339Ptr(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// rfc3339 renders a time as an RFC3339 (UTC) string (always present).
func rfc3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// strPtr returns nil for an empty string, else a pointer to it (empty -> JSON null).
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// decPtrStr renders an optional decimal as a *string (full precision), nil-safe.
func decPtrStr(d *decimal.Decimal) *string {
	if d == nil {
		return nil
	}
	s := d.String()
	return &s
}

// poolLabeler resolves a pool address to its human label (e.g. "USDC/USDe"). The
// server holds a snapshot map; an unknown address yields "".
type poolLabeler func(addr string) string

// signalToDTO maps a store.SignalRow (signal + optional outcome) to the SignalDTO.
// label resolves the pool label; explorerBase builds tx/address links (empty
// disables links, yielding nil URLs).
func signalToDTO(r store.SignalRow, label poolLabeler, explorerBase string) SignalDTO {
	s := r.Signal

	// Triggering-swap tx hash from the internal event_ref (the log index stays
	// internal). Empty for flow (type 1), so both tx_hash and tx_url stay null.
	txHash := txHashFromEventRef(s.EventRef)

	// "go interact" CTA destination for protocol signals (subject = the borrowed reserve
	// for big_borrow, the LST token for lst_flow); "" for DEX types (1/2/3) + depeg.
	au, al := actionURL(s.SignalType, s.Pool)

	dto := SignalDTO{
		ID:        s.ID,
		CreatedAt: rfc3339(s.CreatedAt),
		Type:      s.SignalType,
		TypeName:  signalTypeName(s.SignalType),
		Pool:      s.Pool,
		PoolLabel: label(s.Pool),
		Metric:    s.Metric,
		// Display-only mirror of the on-chain integer score; attest.ScoreFromZ also
		// computes the value attested on-chain, so the two cannot diverge.
		Score:     int(attest.ScoreFromZ(s.Zscore)),
		Zscore:    s.Zscore,
		Actor:     strPtr(s.Actor),
		Note:      strPtr(s.LLMNote),
		AttestTx:  strPtr(s.AttestTx),
		AttestURL: txURL(explorerBase, s.AttestTx),
		Status:    s.Status,
		BucketTS:  rfc3339Ptr(s.BucketTS),

		// Link fields for the frontend.
		PoolURL:  addrURLStr(explorerBase, s.Pool),
		ActorURL: addrURLPtr(explorerBase, s.Actor),
		TxHash:   strPtr(txHash),
		TxURL:    txURL(explorerBase, txHash),

		// Display-only approximate USD size (null when absent); never a detection input.
		SizeUSD: decPtrStr(s.SizeUSD),

		// "go interact" CTA for protocol signals (null for DEX types + depeg).
		ActionURL:   strPtr(au),
		ActionLabel: strPtr(al),
	}

	// Embed the graded outcome when present (the LEFT JOIN populated all four).
	if r.OutcomeGrade != nil && r.OutcomeLetter != nil && r.OutcomeStatus != nil {
		out := &OutcomeDTO{
			Grade:  *r.OutcomeGrade,
			Letter: *r.OutcomeLetter,
			Status: *r.OutcomeStatus,
		}
		if r.OutcomeAttestTx != nil && *r.OutcomeAttestTx != "" {
			out.AttestTx = r.OutcomeAttestTx
			out.AttestURL = txURL(explorerBase, *r.OutcomeAttestTx)
		}
		dto.Outcome = out
	}

	return dto
}

// signalToDetailDTO maps a SignalRow to the full detail DTO (signal + payload + window +
// optional decoded swap). Payload passes through verbatim (empty -> "{}"). swap is
// pre-decoded by the handler (which has the DB + registry), so mapping stays I/O-free.
func signalToDetailDTO(r store.SignalRow, label poolLabeler, explorerBase string, swap *SwapActionDTO) SignalDetailDTO {
	payload := r.Signal.Payload
	if len(payload) == 0 {
		payload = jsonRaw("{}")
	}
	return SignalDetailDTO{
		SignalDTO:     signalToDTO(r, label, explorerBase),
		Payload:       payload,
		WindowBuckets: r.Signal.WindowBuckets,
		Swap:          swap,
	}
}

// signalsToDTOs maps a slice of SignalRows to DTOs (never nil; an empty input
// yields an empty, non-nil slice so the JSON is [] not null).
func signalsToDTOs(rows []store.SignalRow, label poolLabeler, explorerBase string) []SignalDTO {
	out := make([]SignalDTO, 0, len(rows))
	for i := range rows {
		out = append(out, signalToDTO(rows[i], label, explorerBase))
	}
	return out
}

// actorToDTO maps a store.ActorRow to the ActorDTO.
func actorToDTO(a store.ActorRow, explorerBase string) ActorDTO {
	return ActorDTO{
		Actor:           a.Actor,
		ActorURL:        addrURLStr(explorerBase, a.Actor),
		TotalSignals:    a.TotalSignals,
		LargeSwaps24h:   a.LargeSwaps24h,
		PoolsTouched24h: a.PoolsTouched24h,
		LatestScore:     a.LatestScore,
		BestScore:       a.BestScore,
		Grades:          GradesDTO{Count: a.GradesCount, Avg: a.GradesAvg},
		FirstSeen:       rfc3339(a.FirstSeen),
		LastSeen:        rfc3339(a.LastSeen),
	}
}

// poolToDTO maps a store.PoolRow (registry + 24h activity + external stats) to the
// PoolDTO, keeping the activity and stats blocks distinct. tokenMeta is the display-only
// symbol/logo lookup keyed by lowercase token address; a nil or partial map yields ""
// symbols / null logos (never an error).
func poolToDTO(p store.PoolRow, explorerBase string, tokenMeta map[string]store.TokenMeta) PoolDTO {
	dto := PoolDTO{
		Address:  p.Pool.Address,
		URL:      addrURLStr(explorerBase, p.Pool.Address),
		Dex:      p.Pool.Dex,
		Label:    p.Pool.Label,
		Token0:   p.Pool.Token0,
		Token1:   p.Pool.Token1,
		Dec0:     p.Pool.Dec0,
		Dec1:     p.Pool.Dec1,
		Enabled:  p.Pool.Enabled,
		TradeURL: matchaTradeURL(p.Pool.Token0, p.Pool.Token1),
		DexURL:   dexAppURL(p.Pool.Dex),
		LPURL:    lpURL(p.Pool.Dex, p.Pool.Token0, p.Pool.Token1, p.Pool.BinStep),
		Activity: PoolActivityDTO{
			Swaps24h:     p.Swaps24h,
			Vol0_24h:     p.Vol0_24h.String(),
			Vol1_24h:     p.Vol1_24h.String(),
			LastBucketTS: timePtr(p.LastBucketTS),
		},
		RecentSignals24h: p.RecentSignals24h,
	}

	// Join token_meta by token0/token1 address for symbols/logos; absent rows leave the
	// "" / null defaults.
	if tm, ok := tokenMeta[strings.ToLower(p.Pool.Token0)]; ok {
		dto.Token0Symbol = tm.Symbol
		dto.Token0LogoURL = strPtr(tm.LogoURL)
	}
	if tm, ok := tokenMeta[strings.ToLower(p.Pool.Token1)]; ok {
		dto.Token1Symbol = tm.Symbol
		dto.Token1LogoURL = strPtr(tm.LogoURL)
	}
	if p.Stats != nil {
		dto.Stats = &PoolStatsDTO{
			TVLUSD:         decPtrStr(p.Stats.TVLUSD),
			Vol24hUSD:      decPtrStr(p.Stats.Vol24hUSD),
			PriceUSD:       decPtrStr(p.Stats.PriceUSD),
			PriceChange24h: decPtrStr(p.Stats.PriceChange24h),
			FetchedAt:      rfc3339(p.Stats.FetchedAt),
			Source:         p.Stats.SourceURL,
		}
	}
	return dto
}

// swapActionFromLog decodes a signal's triggering swap log into a SwapActionDTO for the
// detail endpoint, via aggregate.DecodeSwap (the authoritative decoder) then
// swapBodyFromNets for the directional in/out. Returns nil when the log cannot be
// decoded or the direction is indeterminate (equal nets), where the endpoint serializes
// swap:null. pool supplies token addresses, authoritative decimals, and dex; tokenMeta
// supplies symbols/logos.
func swapActionFromLog(l store.RawLog, pool store.Pool, tokenMeta map[string]store.TokenMeta, explorerBase string) *SwapActionDTO {
	amt, err := aggregate.DecodeSwap(l.Topic0, l.Data)
	if err != nil {
		// Undecodable (short/odd payload or unknown topic): no action to show.
		return nil
	}
	body, ok := swapBodyFromNets(amt.Netflow0, amt.Netflow1, pool, tokenMeta)
	if !ok {
		return nil
	}
	return &SwapActionDTO{
		Dex:         pool.Dex,
		TokenIn:     body.TokenIn,
		TokenOut:    body.TokenOut,
		AmountInRaw: body.AmountInRaw,
		AmountOut:   body.AmountOut,
		TxURL:       txURL(explorerBase, strings.ToLower(l.TxHash)),
	}
}

// swapBodyFromNets derives the directional core of a swap (token_in sent / token_out
// received + raw amounts) from the signed net flow of token0/token1, from the actor's
// perspective: the more-negative net was sent (token_in), the more-positive was received
// (token_out). This is the API's one sign-convention authority and matches
// aggregate.DecodeSwap's Netflow and the large_swaps ledger's net0/net1, so the actor
// activity path reuses it without re-reading raw_logs. ok=false only when the nets are
// equal (e.g. both zero), where no direction can be shown. Each token's symbol, decimals,
// and logo come from token_meta, falling back to the pool label for the symbol and
// pools.dec0/dec1 for decimals.
func swapBodyFromNets(net0, net1 decimal.Decimal, pool store.Pool, tokenMeta map[string]store.TokenMeta) (SwapBodyDTO, bool) {
	if net0.Equal(net1) {
		// Indeterminate direction (covers both-zero): no meaningful action.
		return SwapBodyDTO{}, false
	}

	tok0 := swapTokenDTO(pool.Token0, pool.Dec0, poolLabelSide(pool.Label, 0), tokenMeta)
	tok1 := swapTokenDTO(pool.Token1, pool.Dec1, poolLabelSide(pool.Label, 1), tokenMeta)

	if net0.LessThan(net1) {
		return SwapBodyDTO{
			TokenIn:     tok0,
			TokenOut:    tok1,
			AmountInRaw: net0.Abs().String(),
			AmountOut:   net1.Abs().String(),
		}, true
	}
	return SwapBodyDTO{
		TokenIn:     tok1,
		TokenOut:    tok0,
		AmountInRaw: net1.Abs().String(),
		AmountOut:   net0.Abs().String(),
	}, true
}

// swapTokenDTO builds one SwapTokenDTO for a token slot: address + authoritative
// decimals from the pool registry, symbol/logo from token_meta when present. fallbackSym
// (the pool-label side) supplies the symbol when token_meta has no row or empty symbol.
func swapTokenDTO(addr string, dec int, fallbackSym string, tokenMeta map[string]store.TokenMeta) SwapTokenDTO {
	out := SwapTokenDTO{
		Address:  strings.ToLower(addr),
		Symbol:   fallbackSym,
		Decimals: dec,
		LogoURL:  nil,
	}
	if tm, ok := tokenMeta[strings.ToLower(addr)]; ok {
		if tm.Symbol != "" {
			out.Symbol = tm.Symbol
		}
		out.LogoURL = strPtr(tm.LogoURL)
	}
	return out
}

// poolLabelSide returns side `i` (0 or 1) of a "TOKEN0/TOKEN1" pool label as the
// fallback token symbol, or "" when the label is missing or not two-part. It splits on
// the first "/" only, so a malformed label cannot over-split.
func poolLabelSide(label string, i int) string {
	label = strings.TrimSpace(label)
	if label == "" {
		return ""
	}
	slash := strings.IndexByte(label, '/')
	if slash < 0 {
		return ""
	}
	left := strings.TrimSpace(label[:slash])
	right := strings.TrimSpace(label[slash+1:])
	if i == 0 {
		return left
	}
	return right
}

// actorSwapToDTO maps one of an actor's recent large swaps to a decoded ActorSwapDTO for
// the activity path. Direction + amounts come from the ledger's signed net0/net1 via
// swapBodyFromNets (no raw-log re-read). Returns ok=false on indeterminate direction
// (equal nets) so the caller can skip it rather than render a directionless entry.
func actorSwapToDTO(ls store.LargeSwap, pool store.Pool, label poolLabeler, tokenMeta map[string]store.TokenMeta, explorerBase string) (ActorSwapDTO, bool) {
	body, ok := swapBodyFromNets(ls.Net0, ls.Net1, pool, tokenMeta)
	if !ok {
		return ActorSwapDTO{}, false
	}
	return ActorSwapDTO{
		TxURL:     txURL(explorerBase, strings.ToLower(ls.TxHash)),
		Pool:      strings.ToLower(ls.Pool),
		PoolLabel: label(ls.Pool),
		Dex:       pool.Dex,
		Swap:      body,
		BlockTime: rfc3339(ls.BlockTime),
	}, true
}

// bucketToSeriesDTO maps a store.Bucket to a pool-detail series entry (raw units).
func bucketToSeriesDTO(b store.Bucket) SeriesBucketDTO {
	return SeriesBucketDTO{
		TS:        rfc3339(b.TsBucket),
		SwapCount: b.SwapCount,
		Vol0:      b.Vol0.String(),
		Vol1:      b.Vol1.String(),
	}
}

// statsToDTO maps a store.StatsSummary to the frozen StatsDTO, stamping UpdatedAt
// with now.
func statsToDTO(s store.StatsSummary, now time.Time) StatsDTO {
	return StatsDTO{
		TotalSignals: s.TotalSignals,
		ByType: ByTypeDTO{
			Flow:        s.ByTypeFlow,
			Whale:       s.ByTypeWhale,
			SmartMoney:  s.ByTypeSmartMoney,
			LSTFlow:     s.ByTypeLSTFlow,
			Liquidation: s.ByTypeLiquidation,
			BigBorrow:   s.ByTypeBigBorrow,
			Depeg:       s.ByTypeDepeg,
		},
		SignalAttestations:  s.SignalAttestations,
		OutcomeAttestations: s.OutcomeAttestations,
		OutcomesTotal:       s.OutcomesTotal,
		GradeDistribution: GradeDistributionDTO{
			A: s.GradeA, B: s.GradeB, C: s.GradeC, D: s.GradeD, F: s.GradeF,
		},
		AvgGrade: s.AvgGrade,
		Last24h: Last24hDTO{
			Signals:       s.Last24hSignals,
			Flow:          s.Last24hFlow,
			Whale:         s.Last24hWhale,
			SmartMoney:    s.Last24hSmartMoney,
			LSTFlow:       s.Last24hLSTFlow,
			Liquidation:   s.Last24hLiquidation,
			BigBorrow:     s.Last24hBigBorrow,
			Depeg:         s.Last24hDepeg,
			TopActor:      strPtr(s.Last24hTopActor),
			OutcomesTotal: s.Last24hOutcomesTotal,
			GradeDistribution: GradeDistributionDTO{
				A: s.Last24hGradeA, B: s.Last24hGradeB, C: s.Last24hGradeC,
				D: s.Last24hGradeD, F: s.Last24hGradeF,
			},
			AvgGrade: s.Last24hAvgGrade,
		},
		PoolsTracked:  s.PoolsTracked,
		FirstSignalAt: rfc3339Ptr(s.FirstSignalAt),
		UpdatedAt:     rfc3339(now),
	}
}

// timePtr renders an optional time as an RFC3339 (UTC) string pointer (nil-safe).
func timePtr(t *time.Time) *string {
	if t == nil || t.IsZero() {
		return nil
	}
	return rfc3339Ptr(*t)
}

// txURL builds an explorer transaction link, or nil when either the base or the
// hash is empty (so the JSON field is null, not a broken link).
func txURL(base, tx string) *string {
	if base == "" || tx == "" {
		return nil
	}
	s := base + "/tx/" + tx
	return &s
}

// addrURLStr builds an explorer address link, or "" when base/addr is empty.
// Returns a plain string (not a pointer) for fields the contract types as string.
func addrURLStr(base, addr string) string {
	if base == "" || addr == "" {
		return ""
	}
	return base + "/address/" + addr
}

// addrURLPtr builds an explorer address link as a *string, or nil when base/addr is
// empty. Used for the nullable actor_url (flow/whale carry no actor, so it serializes
// as JSON null, not a broken link).
func addrURLPtr(base, addr string) *string {
	if base == "" || addr == "" {
		return nil
	}
	s := base + "/address/" + addr
	return &s
}

// matchaTradeURL builds a deep link to swap token0 -> token1 on Matcha, the Mantle swap
// aggregator (gasless). Pattern (verified against live URLs):
// /tokens/mantle/{sell}?buyChain=5000&buyAddress={buy}. "" when either token is empty.
// An action link, independent of explorerBase (a fixed external app).
func matchaTradeURL(token0, token1 string) string {
	t0 := strings.ToLower(strings.TrimSpace(token0))
	t1 := strings.ToLower(strings.TrimSpace(token1))
	if t0 == "" || t1 == "" {
		return ""
	}
	return "https://matcha.xyz/tokens/mantle/" + t0 + "?buyChain=5000&buyAddress=" + t1
}

// dexAppURL maps a pool's DEX to its native app. Only DEXs whose app URL is verified are
// mapped; an unknown or unverified DEX (e.g. fusionx, whose URL pattern we will not
// guess) yields "" (the frontend still has the MantleScan verify + Matcha trade links).
func dexAppURL(dex string) string {
	switch strings.ToLower(strings.TrimSpace(dex)) {
	case "merchant_moe", "merchantmoe", "moe":
		return "https://merchantmoe.com/"
	case "agni", "agni_finance":
		return "https://agni.finance/swap"
	default:
		return ""
	}
}

// wmntAddress is WMNT's verified Mantle contract address (lowercase; matches the anchor
// in internal/discover and config/pools.yaml). On Merchant Moe's v22 router WMNT renders
// as the literal "MNT" (native MNT); see lpURL. Agni uses the address as-is.
const wmntAddress = "0x78c1b0c915c4faa5fffa6cabf0219da63d7f4cb8"

// lpURL builds a deep-link to a pool's add-liquidity page on its native DEX (a third
// action link beside dexAppURL and matchaTradeURL). The patterns are verified, never
// guessed:
//
//   - agni: https://agni.finance/add/{token0}/{token1} (lowercased registry addresses).
//   - merchant_moe: https://merchantmoe.com/pool/v22/{X}/{Y}/{binStep}, X/Y = token0/
//     token1 (registry order, equal to on-chain getTokenX/getTokenY), each WMNT side
//     rendered as the literal "MNT". binStep is pools.bin_step (via getBinStep()); nil
//     returns "" so the frontend falls back to dex_url.
//   - any other dex: "".
//
// "" too when either token is empty. binStep comes from the already-loaded row, so lpURL
// does no I/O.
func lpURL(dex, token0, token1 string, binStep *int) string {
	t0 := strings.ToLower(strings.TrimSpace(token0))
	t1 := strings.ToLower(strings.TrimSpace(token1))
	if t0 == "" || t1 == "" {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(dex)) {
	case "agni", "agni_finance":
		return "https://agni.finance/add/" + t0 + "/" + t1
	case "merchant_moe", "merchantmoe", "moe":
		if binStep == nil {
			return ""
		}
		return "https://merchantmoe.com/pool/v22/" + moeToken(t0) + "/" + moeToken(t1) +
			"/" + strconv.Itoa(*binStep)
	default:
		return ""
	}
}

// moeToken renders one token for a Merchant Moe v22 deep-link: the literal "MNT" when
// the (lowercased) address is WMNT (native MNT), otherwise the address unchanged.
func moeToken(addrLower string) string {
	if addrLower == wmntAddress {
		return "MNT"
	}
	return addrLower
}

// actionURL returns the "go interact" destination + CTA label for a protocol signal
// (verified URLs, never guessed; "","" for DEX types 1/2/3 and depeg 7). subject is the
// attest subject: the borrowed reserve for big_borrow, the LST token for lst_flow.
//   - 6 big_borrow  -> the Aave V3 Mantle reserve-overview page for the reserve
//   - 5 liquidation -> the Aave V3 Mantle market (subject is the liquidated user)
//   - 4 lst_flow    -> the mETH Protocol staking app
//
// Market name "proto_mantle_v3" and host methprotocol.xyz are both verified.
func actionURL(signalType int16, subject string) (url, label string) {
	switch signalType {
	case store.SignalTypeBigBorrow: // subject = the borrowed reserve token
		s := strings.ToLower(strings.TrimSpace(subject))
		if s == "" {
			return "https://app.aave.com/markets/?marketName=proto_mantle_v3", "Aave market"
		}
		return "https://app.aave.com/reserve-overview/?underlyingAsset=" + s + "&marketName=proto_mantle_v3", "Borrow on Aave"
	case store.SignalTypeLiquidation: // subject is the liquidated user; link to the market
		return "https://app.aave.com/markets/?marketName=proto_mantle_v3", "View on Aave"
	case store.SignalTypeLSTFlow: // the mETH/cmETH staking / restaking app
		return "https://www.methprotocol.xyz/", "Stake on mETH Protocol"
	default:
		return "", ""
	}
}

// txHashFromEventRef extracts the lowercase tx-hash portion (before the first ':') from
// a signal's internal event_ref "txhash:logindex", or "" when empty/hash-less so a flow
// signal's tx_hash / tx_url stay null. Only the hash is surfaced; the log index stays
// internal.
func txHashFromEventRef(eventRef string) string {
	ref := strings.TrimSpace(eventRef)
	if ref == "" {
		return ""
	}
	if i := strings.IndexByte(ref, ':'); i >= 0 {
		return strings.ToLower(strings.TrimSpace(ref[:i]))
	}
	// No separator: treat the whole ref as the hash (defensive; detectors always write
	// "txhash:logindex").
	return strings.ToLower(ref)
}
