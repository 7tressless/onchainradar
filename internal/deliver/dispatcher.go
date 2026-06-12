// dispatcher.go composes a store.Signal into the Telegram channel post: a branded PNG
// card (internal/deliver/card), an HTML caption aimed at a general web2/web3 reader (a
// single type emoji, plain-language facts with bold key numbers, never a raw z-score,
// an expandable analyst note, and one tap-to-copy artifact), and a row of inline URL
// buttons. Every dynamic caption field is HTML-escaped; card fields are plain text.
package deliver

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"ocr/internal/deliver/card"
	"ocr/internal/store"
)

// Signal family discriminators (mirror store.SignalType*). They pick the per-family
// renderer; the numbers go into the immutable on-chain record, so they never change.
const (
	whaleSignalType       = 2
	smartMoneySignalType  = 3
	lstFlowSignalType     = 4
	liquidationSignalType = 5
	bigBorrowSignalType   = 6
	depegSignalType       = 7
)

// maxNoteRunes caps the analyst note inside the caption. The Bot API limits a caption to
// 1024 visible characters; every other part is bounded by construction (shortened
// addresses, a capped exposed-pools list), so clamping the one unbounded field keeps the
// whole caption within the limit. TestCaption_BudgetWithLongNote pins this.
const maxNoteRunes = 700

// MessageView is a fully-rendered alert: the card image data, the HTML caption, and
// the optional inline keyboard. The async enrich stage re-renders the same view (now
// with the note) and edits the caption, re-sending the keyboard because Telegram drops
// a message's buttons on any edit without reply_markup.
type MessageView struct {
	Caption  string
	Keyboard *inlineKeyboard
	Card     card.Data
}

// Context carries the readable pool metadata the renderer needs but the raw signal
// does not: the human pair label and the DEX name. Both may be empty when the pool is
// unknown, in which case the renderers fall back to the short pool address.
type Context struct {
	PoolLabel string
	Dex       string
}

// FormatSignal renders a signal into a MessageView, dispatching on signal_type to the
// per-family renderer. attestTx links the on-chain proof button and the card footer
// (empty until the signal is attested); mantleScanBase builds explorer URLs;
// dashboardURL is the public dashboard link.
func FormatSignal(s store.Signal, dc Context, attestTx, mantleScanBase, dashboardURL string) MessageView {
	switch s.SignalType {
	case whaleSignalType:
		return formatWhale(s, dc, attestTx, mantleScanBase, dashboardURL)
	case smartMoneySignalType:
		return formatSmartMoney(s, dc, attestTx, mantleScanBase, dashboardURL)
	case lstFlowSignalType:
		return formatLSTFlow(s, attestTx, mantleScanBase, dashboardURL)
	case liquidationSignalType, bigBorrowSignalType:
		return formatLending(s, attestTx, mantleScanBase, dashboardURL)
	case depegSignalType:
		return formatDepeg(s, attestTx, mantleScanBase, dashboardURL)
	default:
		return formatFlow(s, dc, attestTx, mantleScanBase, dashboardURL)
	}
}

// decodePayload best-effort-decodes a signal's JSON payload into dst. A malformed or
// empty payload leaves dst at its zero value: a renderer degrades to the fields it can
// derive without the payload rather than dropping the alert.
func decodePayload(raw json.RawMessage, dst any) {
	if len(raw) == 0 {
		return
	}
	_ = json.Unmarshal(raw, dst)
}

// formatFlow renders a flow anomaly (type 1): a burst on one metric, stated as how
// many times the latest bucket exceeded the pool's own 48h median.
func formatFlow(s store.Signal, dc Context, attestTx, base, dashboardURL string) MessageView {
	var p struct {
		Latest float64 `json:"latest"`
		Median float64 `json:"median"`
	}
	decodePayload(s.Payload, &p)
	mult, hasMult := humanMultiple(p.Latest, p.Median)
	noun := metricLabel(s.Metric)

	var body strings.Builder
	if hasMult {
		fmt.Fprintf(&body, "%s at <b>%s</b> this pool's own 48-hour average.",
			capitalize(noun), mult)
	} else {
		fmt.Fprintf(&body, "Unusual %s, far above this pool's own baseline.", noun)
	}

	sub := []card.Run{{Text: capitalize(noun), Bold: true}}
	if hasMult {
		sub = append(sub,
			card.Run{Text: " at ", Tone: card.ToneSub},
			card.Run{Text: mult, Tone: card.ToneType, Bold: true},
			card.Run{Text: " the 48h baseline", Tone: card.ToneSub},
		)
	} else {
		sub = append(sub, card.Run{Text: " far above the 48h baseline", Tone: card.ToneSub})
	}

	specs := []card.Spec{{Label: "METRIC", Value: strings.ToUpper(noun)}}
	if hasMult {
		specs = append(specs, card.Spec{Label: "VS 48H MEDIAN", Value: mult, Hot: true})
	}
	specs = append(specs, venueSpec(dc, s.Pool))

	return MessageView{
		Caption:  caption("📊", flowTitle(s.Metric), pairSubject(dc.PoolLabel, s.Pool), dexVenue(dc), body.String(), s.LLMNote, "Pool", s.Pool, signalUnix(s)),
		Keyboard: signalButtons("View pool", s.Pool, "", attestTx, base, dashboardURL),
		Card:     cardData(s, "flow", flowTypeLabel(s.Metric), pairHeadline(dc.PoolLabel, s.Pool), sub, specs, attestTx),
	}
}

// formatWhale renders a single large outlier swap (type 2): the dollar size when
// known, the multiple of the pool's usual trade, and the firing wallet (tx.origin).
func formatWhale(s store.Signal, dc Context, attestTx, base, dashboardURL string) MessageView {
	var p struct {
		Size0  float64 `json:"size0"`
		Median float64 `json:"median"`
	}
	decodePayload(s.Payload, &p)
	usd := usdSize(s)
	mult, hasMult := humanMultiple(p.Size0, p.Median)
	actor := strings.TrimSpace(s.Actor)

	var body strings.Builder
	switch {
	case usd != "" && hasMult:
		fmt.Fprintf(&body, "<b>%s</b> swap, <b>%s</b> the pool's usual trade size.", usd, mult)
	case usd != "":
		fmt.Fprintf(&body, "<b>%s</b> swap, far above the pool's usual trade size.", usd)
	case hasMult:
		fmt.Fprintf(&body, "One trade at <b>%s</b> the pool's usual size.", mult)
	default:
		body.WriteString("One trade far above the pool's usual size.")
	}

	var sub []card.Run
	if actor != "" {
		sub = []card.Run{
			{Text: "Wallet ", Tone: card.ToneSub},
			{Text: shortAddr(actor), Bold: true},
			{Text: " · one outsized trade", Tone: card.ToneFaint},
		}
	} else {
		sub = []card.Run{{Text: "One outsized trade on this pool", Tone: card.ToneSub}}
	}

	var specs []card.Spec
	if v := usdCompact(s); v != "" {
		specs = append(specs, card.Spec{Label: "NOTIONAL", Value: v})
	}
	if hasMult {
		specs = append(specs, card.Spec{Label: "VS POOL MEDIAN", Value: mult, Hot: true})
	}
	specs = append(specs, venueSpec(dc, s.Pool))

	copyLabel, copyAddr := "Wallet", actor
	if actor == "" {
		copyLabel, copyAddr = "Pool", s.Pool
	}
	return MessageView{
		Caption:  caption("🐋", "Whale swap", pairSubject(dc.PoolLabel, s.Pool), dexVenue(dc), body.String(), s.LLMNote, copyLabel, copyAddr, signalUnix(s)),
		Keyboard: signalButtons("View pool", s.Pool, actor, attestTx, base, dashboardURL),
		Card:     cardData(s, "whale", "WHALE SWAP", pairHeadline(dc.PoolLabel, s.Pool), sub, specs, attestTx),
	}
}

// formatSmartMoney renders an accumulating wallet (type 3): the 0..100 accumulation
// score plus the evidence (repeat large buys across pools). A payload decode failure
// still yields a valid alert (score only).
func formatSmartMoney(s store.Signal, dc Context, attestTx, base, dashboardURL string) MessageView {
	var p struct {
		Actor         string  `json:"actor"`
		AccumSwaps    int     `json:"accum_swaps"`
		DistinctPools int     `json:"distinct_pools"`
		SizeMultiple  float64 `json:"size_multiple"`
	}
	decodePayload(s.Payload, &p)
	actor := strings.TrimSpace(s.Actor)
	if actor == "" {
		actor = strings.TrimSpace(p.Actor)
	}
	score := fmt.Sprintf("%.0f", s.Zscore)

	var body strings.Builder
	if p.AccumSwaps > 0 {
		fmt.Fprintf(&body, "One wallet accumulating: <b>%d large buys</b> across <b>%d pool(s)</b>, about <b>%.0f×</b> its usual size. Score <b>%s/100</b>.",
			p.AccumSwaps, p.DistinctPools, p.SizeMultiple, score)
	} else {
		fmt.Fprintf(&body, "One wallet on a repeat accumulation pattern. Score <b>%s/100</b>.", score)
	}

	var sub []card.Run
	if p.AccumSwaps > 0 {
		sub = []card.Run{
			{Text: fmt.Sprintf("%d large buys", p.AccumSwaps), Bold: true},
			{Text: " across ", Tone: card.ToneSub},
			{Text: fmt.Sprintf("%d pool(s)", p.DistinctPools), Bold: true},
			{Text: " · net accumulation", Tone: card.ToneSub},
		}
	} else {
		sub = []card.Run{{Text: "Repeat accumulation pattern", Tone: card.ToneSub}}
	}

	specs := []card.Spec{{Label: "SMART SCORE", Value: score, Hot: true}}
	if p.AccumSwaps > 0 {
		specs = append(specs,
			card.Spec{Label: "LARGE BUYS", Value: strconv.Itoa(p.AccumSwaps)},
			card.Spec{Label: "POOLS", Value: strconv.Itoa(p.DistinctPools)},
		)
	} else {
		specs = append(specs, venueSpec(dc, s.Pool))
	}

	return MessageView{
		Caption:  caption("🧠", "Smart money", pairSubject(dc.PoolLabel, s.Pool), dexVenue(dc), body.String(), s.LLMNote, "Wallet", actor, signalUnix(s)),
		Keyboard: signalButtons("View pool", s.Pool, actor, attestTx, base, dashboardURL),
		Card:     cardData(s, "smart", "SMART MONEY", pairHeadline(dc.PoolLabel, s.Pool), sub, specs, attestTx),
	}
}

// formatLSTFlow renders a large mETH/cmETH transfer (type 4), classified by
// counterparty into staked in / redeemed / moved.
func formatLSTFlow(s store.Signal, attestTx, base, dashboardURL string) MessageView {
	var p struct {
		Symbol    string `json:"symbol"`
		Direction string `json:"direction"`
		ValueLST  string `json:"value_lst"`
	}
	decodePayload(s.Payload, &p)
	sym := p.Symbol
	if sym == "" {
		sym = "LST"
	}
	verb, emoji, tagline, dirLabel := "moved", "🔁", "wallet to wallet", "MOVED"
	switch p.Direction {
	case "mint":
		verb, emoji, tagline, dirLabel = "staked in", "🟢", "fresh ETH into Mantle", "STAKED IN"
	case "burn":
		verb, emoji, tagline, dirLabel = "redeemed", "🔴", "ETH leaving Mantle", "REDEEMED"
	}
	actor := strings.TrimSpace(s.Actor)
	amount := groupAmount(p.ValueLST)

	var body strings.Builder
	if p.ValueLST != "" {
		fmt.Fprintf(&body, "<b>%s %s</b>, %s.", EscapeHTML(amount), EscapeHTML(sym), tagline)
	} else {
		fmt.Fprintf(&body, "A large <b>%s</b> transfer, %s.", EscapeHTML(sym), tagline)
	}

	var sub []card.Run
	if p.ValueLST != "" {
		sub = []card.Run{
			{Text: amount + " " + sym, Bold: true},
			{Text: " · " + tagline, Tone: card.ToneSub},
		}
	} else {
		sub = []card.Run{{Text: capitalize(verb) + " · " + tagline, Tone: card.ToneSub}}
	}

	var specs []card.Spec
	if p.ValueLST != "" {
		specs = append(specs, card.Spec{Label: "AMOUNT", Value: specAmount(p.ValueLST)})
	}
	specs = append(specs,
		card.Spec{Label: "DIRECTION", Value: dirLabel, Hot: true},
		card.Spec{Label: "NETWORK", Value: "MANTLE"},
	)

	copyLabel, copyAddr := "Wallet", actor
	if actor == "" {
		copyLabel, copyAddr = "Token", s.Pool
	}
	return MessageView{
		Caption:  caption(emoji, EscapeHTML(sym)+" "+verb, "Mantle", "", body.String(), s.LLMNote, copyLabel, copyAddr, signalUnix(s)),
		Keyboard: signalButtons("View token", s.Pool, actor, attestTx, base, dashboardURL),
		Card:     cardData(s, "lst", "LST FLOW", sym, sub, specs, attestTx),
	}
}

// formatLending renders an Aave V3 liquidation (type 5) or big borrow (type 6): the
// dollar size with the event in plain words, and the controlling wallet.
func formatLending(s store.Signal, attestTx, base, dashboardURL string) MessageView {
	var p struct {
		Liquidator string `json:"liquidator"`
		OnBehalfOf string `json:"on_behalf_of"`
	}
	decodePayload(s.Payload, &p)

	isLiquidation := s.SignalType == liquidationSignalType
	emoji, title, line, who, subjectLabel := "💵", "Big borrow", "borrowed", "Borrower", "View reserve"
	kind, typeLabel, eventLabel := "borrow", "BIG BORROW", "BORROW"
	if isLiquidation {
		emoji, title, line, who, subjectLabel = "⚡", "Liquidation", "position liquidated", "Liquidator", "View borrower"
		kind, typeLabel, eventLabel = "liq", "LIQUIDATION", "LIQUIDATION"
	}
	actor := strings.TrimSpace(s.Actor)
	if actor == "" {
		if isLiquidation {
			actor = strings.TrimSpace(p.Liquidator)
		} else {
			actor = strings.TrimSpace(p.OnBehalfOf)
		}
	}

	var body strings.Builder
	if usd := usdSize(s); usd != "" {
		fmt.Fprintf(&body, "<b>%s</b> %s on Aave V3.", usd, line)
	} else {
		fmt.Fprintf(&body, "Large %s on Aave V3.", line)
	}

	var sub []card.Run
	if usd := usdSize(s); usd != "" {
		sub = []card.Run{{Text: usd, Bold: true}, {Text: " " + line, Tone: card.ToneSub}}
	} else {
		sub = []card.Run{{Text: capitalize(line), Tone: card.ToneSub}}
	}
	if actor != "" {
		sub = append(sub, card.Run{Text: " · " + shortAddr(actor), Tone: card.ToneFaint})
	}

	var specs []card.Spec
	if v := usdCompact(s); v != "" {
		specs = append(specs, card.Spec{Label: "SIZE", Value: v})
	}
	specs = append(specs,
		card.Spec{Label: "EVENT", Value: eventLabel, Hot: true},
		card.Spec{Label: "VENUE", Value: "AAVE V3"},
	)

	copyLabel, copyAddr := who, actor
	if actor == "" {
		copyLabel, copyAddr = "Reserve", s.Pool
	}
	return MessageView{
		Caption:  caption(emoji, title, "Aave V3", "", body.String(), s.LLMNote, copyLabel, copyAddr, signalUnix(s)),
		Keyboard: signalButtons(subjectLabel, s.Pool, actor, attestTx, base, dashboardURL),
		Card:     cardData(s, kind, typeLabel, "AAVE V3", sub, specs, attestTx),
	}
}

// formatDepeg renders a stablecoin off its $1 peg (type 7): the implied price from our
// own decoded swaps, the deviation in bps, and the exposed pools.
func formatDepeg(s store.Signal, attestTx, base, dashboardURL string) MessageView {
	var p struct {
		TokenSymbol  string `json:"token_symbol"`
		ImpliedPrice string `json:"implied_price"`
		DeviationBps int    `json:"deviation_bps"`
		Affected     []struct {
			Label   string `json:"label"`
			Address string `json:"address"`
		} `json:"affected_pools"`
	}
	decodePayload(s.Payload, &p)
	sym := p.TokenSymbol
	if sym == "" {
		sym = "stablecoin"
	}
	pct := fmt.Sprintf("%g%%", float64(p.DeviationBps)/100)

	var body strings.Builder
	if p.ImpliedPrice != "" {
		fmt.Fprintf(&body, "%s trading at <b>$%s</b>, <b>%d bps below</b> the $1 peg.",
			EscapeHTML(sym), EscapeHTML(p.ImpliedPrice), p.DeviationBps)
	} else {
		fmt.Fprintf(&body, "%s trading <b>%d bps below</b> the $1 peg.", EscapeHTML(sym), p.DeviationBps)
	}
	if labels := affectedLabels(p.Affected); labels != "" {
		body.WriteString("\nExposed: " + labels + ".")
	}

	var sub []card.Run
	if p.ImpliedPrice != "" {
		sub = []card.Run{
			{Text: "Trading at ", Tone: card.ToneSub},
			{Text: "$" + p.ImpliedPrice, Bold: true},
			{Text: " · ", Tone: card.ToneFaint},
			{Text: fmt.Sprintf("%d bps below the $1 peg", p.DeviationBps), Tone: card.ToneType, Bold: true},
		}
	} else {
		sub = []card.Run{{Text: fmt.Sprintf("%d bps below the $1 peg", p.DeviationBps), Tone: card.ToneType, Bold: true}}
	}

	specs := []card.Spec{{Label: "DEVIATION", Value: pct, Hot: true}}
	if p.ImpliedPrice != "" {
		specs = append(specs, card.Spec{Label: "IMPLIED", Value: "$" + p.ImpliedPrice})
	}
	if n := len(p.Affected); n > 0 {
		specs = append(specs, card.Spec{Label: "EXPOSED POOLS", Value: strconv.Itoa(n)})
	}

	return MessageView{
		Caption:  caption("⚠️", "Depeg alert", EscapeHTML(sym), "", body.String(), s.LLMNote, "Token", s.Pool, signalUnix(s)),
		Keyboard: signalButtons("View token", s.Pool, "", attestTx, base, dashboardURL),
		Card:     cardData(s, "depeg", "DEPEG ALERT", sym, sub, specs, attestTx),
	}
}

// caption assembles the post caption: a bold title (emoji · type · subject, with an
// italic venue when present) that doubles as the push-notification preview, the
// plain-language body, the analyst note as an expandable blockquote, one tap-to-copy
// artifact, and a native tg-time footer. typ/subject/venue arrive escaped; body arrives
// as ready HTML. ts is the signal's Unix time, or 0 to omit the footer.
func caption(emoji, typ, subject, venue, body, note, copyLabel, copyAddr string, ts int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>%s</b> · <b>%s</b>", emoji, typ, subject)
	if v := strings.TrimSpace(venue); v != "" {
		fmt.Fprintf(&b, " · <i>%s</i>", v)
	}
	fmt.Fprintf(&b, "\n\n%s", body)
	b.WriteString(noteBlock(note))

	var footer string
	if a := strings.TrimSpace(copyAddr); a != "" {
		footer = fmt.Sprintf("%s  <code>%s</code>", copyLabel, EscapeHTML(a))
	}
	if ts > 0 {
		if footer != "" {
			footer += "\n"
		}
		footer += timeFooter(ts)
	}
	if footer != "" {
		fmt.Fprintf(&b, "\n\n%s", footer)
	}
	return b.String()
}

// timeFooter renders the footer timestamp as native tg-time entities — each client shows
// them in its own locale and keeps the relative age live: "{age} · {date}, {clock}". The
// inner text is the fallback for clients that predate the entity.
func timeFooter(ts int64) string {
	t := time.Unix(ts, 0).UTC()
	clock := t.Format("15:04")
	return fmt.Sprintf(
		`<tg-time unix=%d format="r">%s</tg-time> · <tg-time unix=%d format="D">%s</tg-time>, <tg-time unix=%d format="t">%s</tg-time>`,
		ts, clock+" UTC", ts, t.Format("January 2, 2006"), ts, clock,
	)
}

// cardData wraps the per-family card fields with the parts every family derives the same
// way from the signal: the reference id, the attestation footer, and the UTC timestamp.
func cardData(s store.Signal, kind, typeLabel, headline string, sub []card.Run, specs []card.Spec, attestTx string) card.Data {
	sigID := ""
	if s.ID > 0 {
		sigID = "SIG #" + strconv.FormatInt(s.ID, 10)
	}
	return card.Data{
		Kind:          kind,
		TypeLabel:     typeLabel,
		SigID:         sigID,
		Headline:      headline,
		Sub:           sub,
		Specs:         specs,
		AttestTxShort: shortAddr(attestTx),
		Timestamp:     tsLine(s.CreatedAt),
	}
}

// tsLine renders the card footer timestamp, e.g. "12 JUN 2026 · 14:31 UTC".
func tsLine(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return strings.ToUpper(t.UTC().Format("02 Jan 2006 · 15:04")) + " UTC"
}

// pairHeadline renders the card headline for a DEX-pool signal: the pair label with
// breathing room around the slash ("USDe / WMNT"), or the short address fallback.
func pairHeadline(label, pool string) string {
	l := strings.TrimSpace(label)
	if l == "" {
		return shortAddr(pool)
	}
	parts := strings.Split(l, "/")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return strings.Join(parts, " / ")
}

// venueSpec is the trailing card spec cell for DEX signals: the DEX name when known,
// else the short pool address.
func venueSpec(dc Context, pool string) card.Spec {
	if d := strings.TrimSpace(dc.Dex); d != "" {
		return card.Spec{Label: "VENUE", Value: strings.ToUpper(d)}
	}
	return card.Spec{Label: "POOL", Value: shortAddr(pool)}
}

// flowTypeLabel is the card's caps type label for a flow metric.
func flowTypeLabel(metric string) string {
	switch metric {
	case "swap_count":
		return "ACTIVITY SPIKE"
	case "netflow0", "netflow1":
		return "NET-FLOW SPIKE"
	default:
		return "VOLUME SPIKE"
	}
}

// flowTitle is the plain-language caption title for a flow metric.
func flowTitle(metric string) string {
	switch metric {
	case "swap_count":
		return "Activity spike"
	case "netflow0", "netflow1":
		return "Net-flow spike"
	default:
		return "Volume spike"
	}
}

// metricLabel maps a flow metric to a plain-language noun.
func metricLabel(metric string) string {
	switch metric {
	case "swap_count":
		return "swap activity"
	case "netflow0", "netflow1":
		return "net flow"
	default:
		return "volume"
	}
}

// capitalize upper-cases the first letter of a plain ASCII phrase.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// humanMultiple expresses how many times the latest value exceeded the baseline median,
// as a plain "9.1×" / "18×" string a non-technical reader understands (the replacement
// for the z-score). It uses magnitudes so a signed metric still reads as a positive
// multiple, and reports ok=false when there is nothing meaningful to compare against
// (non-positive or non-finite median, or a sub-1× ratio).
func humanMultiple(latest, median float64) (string, bool) {
	if median == 0 || math.IsNaN(median) || math.IsNaN(latest) || math.IsInf(median, 0) {
		return "", false
	}
	m := math.Abs(latest) / math.Abs(median)
	if m < 1 || math.IsInf(m, 0) {
		return "", false
	}
	if m < 10 {
		return fmt.Sprintf("%.1f×", m), true
	}
	return fmt.Sprintf("%.0f×", m), true
}

// pairSubject is the bold caption subject for a DEX-pool signal: the spaced, escaped
// pair (or the short pool address when unlabeled).
func pairSubject(label, pool string) string {
	return EscapeHTML(pairHeadline(label, pool))
}

// dexVenue is the italic caption venue for a DEX-pool signal (escaped), or "" when the
// DEX is unknown.
func dexVenue(dc Context) string {
	return EscapeHTML(strings.TrimSpace(dc.Dex))
}

// signalUnix is the signal's creation time as a Unix timestamp for the tg-time footer,
// or 0 to omit the footer when the time is unset.
func signalUnix(s store.Signal) int64 {
	if s.CreatedAt.IsZero() {
		return 0
	}
	return s.CreatedAt.Unix()
}

// noteBlock renders the analyst note as an expandable blockquote (collapsed in the
// feed, tap to expand) after a blank line, or "" when there is no note yet. The note
// is clamped to the caption budget; the async enrich stage edits the full view in.
func noteBlock(note string) string {
	note = clampRunes(strings.TrimSpace(note), maxNoteRunes)
	if note == "" {
		return ""
	}
	return "\n\n<blockquote expandable><b>Analyst take</b>\n" + EscapeHTML(note) + "</blockquote>"
}

// clampRunes truncates s to at most n runes, marking the cut with an ellipsis.
func clampRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

// usdSize renders the signal's display-only USD notional as "$48,200" (rounded,
// grouped), or "" when absent. The value is our own decoded approximation (size_usd),
// never an external price.
func usdSize(s store.Signal) string {
	if s.SizeUSD == nil {
		return ""
	}
	return "$" + groupThousands(s.SizeUSD.Round(0).IntPart())
}

// usdCompact renders the USD notional in card-spec form ("$48.2K", "$1.2M"), or ""
// when absent.
func usdCompact(s store.Signal) string {
	if s.SizeUSD == nil {
		return ""
	}
	v := s.SizeUSD.InexactFloat64()
	switch {
	case v >= 1e9:
		return fmt.Sprintf("$%.1fB", v/1e9)
	case v >= 1e6:
		return fmt.Sprintf("$%.1fM", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("$%.1fK", v/1e3)
	default:
		return fmt.Sprintf("$%.0f", v)
	}
}

// specAmount renders a token amount for a card spec cell: grouped integer part only,
// so the value stays within the cell at the spec font size.
func specAmount(s string) string {
	intPart, _, _ := strings.Cut(strings.TrimSpace(s), ".")
	return groupAmount(intPart)
}

// affectedLabels joins the contagion pools' labels (address fallback) into a compact
// "a · b · c" string, capped so a wide contagion set cannot blow up the post.
func affectedLabels(pools []struct {
	Label   string `json:"label"`
	Address string `json:"address"`
}) string {
	const limit = 4
	var parts []string
	for i, ap := range pools {
		if i == limit {
			parts = append(parts, fmt.Sprintf("+%d more", len(pools)-limit))
			break
		}
		name := ap.Label
		if name == "" {
			name = shortAddr(ap.Address)
		}
		parts = append(parts, EscapeHTML(name))
	}
	return strings.Join(parts, " · ")
}

// shortAddr abbreviates a hex address to "0x1234…abcd" for inline display; the full
// address lives on a button or in the caption's code block. Hex has no HTML markup
// characters, so the result is safe in both captions and card text. Short or empty
// input is returned unchanged.
func shortAddr(a string) string {
	a = strings.TrimSpace(a)
	if len(a) <= 12 {
		return a
	}
	return a[:6] + "…" + a[len(a)-4:]
}

// groupThousands renders n with comma thousands separators (48200 -> "48,200").
func groupThousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	var out strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		out.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if out.Len() > 0 {
			out.WriteByte(',')
		}
		out.WriteString(s[i : i+3])
	}
	if neg {
		return "-" + out.String()
	}
	return out.String()
}

// groupAmount renders a decimal-string token amount with grouped thousands on the
// integer part (1240 -> "1,240", 12.5 -> "12.5"), and passes through anything that is
// not a plain number (or is too large to group safely) unchanged.
func groupAmount(s string) string {
	s = strings.TrimSpace(s)
	intPart, frac, hasFrac := strings.Cut(s, ".")
	digits := strings.TrimPrefix(intPart, "-")
	if digits == "" {
		return s
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return s
		}
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return s
	}
	out := groupThousands(n)
	if strings.HasPrefix(intPart, "-") {
		out = "-" + out
	}
	if hasFrac {
		out += "." + frac
	}
	return out
}

// signalButtons builds the inline-keyboard URL buttons under a signal alert: the
// on-chain attestation proof (when attested), the subject's explorer page (labelled
// per family), the actor wallet (when resolved), and the live dashboard. A button is
// added only when its URL is usable, and they are laid out two per row. Returns nil
// when nothing renders.
func signalButtons(subjectLabel, subjectAddr, actorAddr, attestTx, base, dashboardURL string) *inlineKeyboard {
	base = strings.TrimRight(strings.TrimSpace(base), "/")

	var btns []inlineButton
	if tx := strings.TrimSpace(attestTx); tx != "" && base != "" {
		btns = append(btns, inlineButton{Text: "On-chain proof", URL: base + "/tx/" + tx})
	}
	if a := strings.TrimSpace(subjectAddr); a != "" && subjectLabel != "" && base != "" {
		btns = append(btns, inlineButton{Text: subjectLabel, URL: base + "/address/" + a})
	}
	if a := strings.TrimSpace(actorAddr); a != "" && base != "" {
		btns = append(btns, inlineButton{Text: "Wallet", URL: base + "/address/" + a})
	}
	if d := strings.TrimSpace(dashboardURL); d != "" {
		btns = append(btns, inlineButton{Text: "Live dashboard", URL: d})
	}
	if len(btns) == 0 {
		return nil
	}
	return &inlineKeyboard{Rows: chunkButtons(btns, 2)}
}

// chunkButtons groups a flat button list into rows of at most n.
func chunkButtons(btns []inlineButton, n int) [][]inlineButton {
	var rows [][]inlineButton
	for i := 0; i < len(btns); i += n {
		end := i + n
		if end > len(btns) {
			end = len(btns)
		}
		rows = append(rows, btns[i:end])
	}
	return rows
}

// Dispatch renders the signal and sends it as a card photo with the caption and
// buttons, returning the sent message's id so the caller can persist it
// (store.UpdateSignalTGMessageID) and later edit the analyst note into the same
// message via EditSignal. The alert is never lost to the image: a render or photo-send
// failure falls back to a plain text send of the same caption (logged loudly), and
// only a failure of both paths returns an error.
func Dispatch(ctx context.Context, tg *Telegram, s store.Signal, dc Context, attestTx, mantleScanBase, dashboardURL string) (string, error) {
	v := FormatSignal(s, dc, attestTx, mantleScanBase, dashboardURL)

	png, rerr := card.Render(v.Card)
	if rerr != nil {
		log.Error().Err(rerr).Int64("signal_id", s.ID).Msg("deliver: card render failed; sending text fallback")
		msgID, err := tg.Send(ctx, v.Caption, v.Keyboard)
		if err != nil {
			return "", fmt.Errorf("deliver: dispatch signal %d for pool %s: %w", s.ID, s.Pool, err)
		}
		return msgID, nil
	}

	msgID, perr := tg.SendPhoto(ctx, png, v.Caption, v.Keyboard)
	if perr == nil {
		return msgID, nil
	}
	if ctx.Err() != nil {
		return "", fmt.Errorf("deliver: dispatch signal %d for pool %s: %w", s.ID, s.Pool, perr)
	}
	log.Warn().Err(perr).Int64("signal_id", s.ID).Msg("deliver: photo send failed; sending text fallback")
	msgID, terr := tg.Send(ctx, v.Caption, v.Keyboard)
	if terr != nil {
		return "", fmt.Errorf("deliver: dispatch signal %d for pool %s: photo: %v; text fallback: %w", s.ID, s.Pool, perr, terr)
	}
	return msgID, nil
}

// EditSignal edits a re-rendered view (now carrying the analyst note) into an
// already-sent alert. Card alerts take editMessageCaption; when Telegram rejects that
// (the alert was delivered via the text fallback, which has no caption), it retries as
// a text edit. Failure of both paths returns one error naming each.
func EditSignal(ctx context.Context, tg *Telegram, messageID string, v MessageView) error {
	capErr := tg.EditMessageCaption(ctx, messageID, v.Caption, v.Keyboard)
	if capErr == nil {
		return nil
	}
	if ctx.Err() != nil {
		return capErr
	}
	textErr := tg.EditMessageText(ctx, messageID, v.Caption, v.Keyboard)
	if textErr == nil {
		return nil
	}
	return fmt.Errorf("deliver: edit message %s: caption: %v; text: %w", messageID, capErr, textErr)
}
