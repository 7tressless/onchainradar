// dispatcher.go turns a store.Signal into a Telegram-ready HTML message and ships it
// through the Telegram client: title, pool link, z-score, optional LLM note, and the
// attestation tx link. Dynamic fields are escaped with EscapeHTML. This is Telegram
// message text, not the dashboard, so emoji are allowed.
package deliver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"ocr/internal/store"
)

// smartMoneySignalType (3) is the accumulation detector's signal_type, framed as a
// 0..100 "score" finding (not a z-score) that names the accumulating wallet.
const smartMoneySignalType = 3

// liquidationSignalType / bigBorrowSignalType are the Aave V3 lending family (5/6),
// framed by event kind plus approximate USD size.
const (
	liquidationSignalType = 5
	bigBorrowSignalType   = 6
)

// lstFlowSignalType (4) is a large mETH/cmETH transfer, framed as a mint/burn/move
// flow with a whole-token amount (no USD).
const lstFlowSignalType = 4

// depegSignalType (7) is a stablecoin off its $1 peg, framed with implied price, bps,
// and the contagion pools.
const depegSignalType = 7

// FormatSignal renders a store.Signal as a Telegram HTML message, dispatching to a
// per-family formatter (types 3/4/5/6/7 below) and handling flow (type 1) / whale
// (type 2) inline with the z-score framing. All dynamic fields are escaped with
// EscapeHTML; mantleScanBase is a trusted, pre-validated URL base.
func FormatSignal(s store.Signal, poolLabel, attestTx, mantleScanBase string) string {
	if s.SignalType == smartMoneySignalType {
		return formatSmartMoney(s, poolLabel, attestTx, mantleScanBase)
	}
	if s.SignalType == lstFlowSignalType {
		return formatLSTFlow(s, attestTx, mantleScanBase)
	}
	if s.SignalType == depegSignalType {
		return formatDepeg(s, attestTx, mantleScanBase)
	}
	if s.SignalType == liquidationSignalType || s.SignalType == bigBorrowSignalType {
		return formatLending(s, attestTx, mantleScanBase)
	}

	base := strings.TrimRight(mantleScanBase, "/")

	metric := EscapeHTML(s.Metric)
	label := EscapeHTML(poolLabel)
	pool := EscapeHTML(s.Pool)

	var b strings.Builder

	// Title: metric + z-score so the alert is scannable in a topic feed.
	fmt.Fprintf(&b, "🛰 <b>%s</b> z=%.2f\n", metric, s.Zscore)

	poolHref := fmt.Sprintf("%s/address/%s", base, pool)
	fmt.Fprintf(&b, "Pool: %s (<a href=\"%s\">%s</a>)\n", label, poolHref, pool)

	fmt.Fprintf(&b, "Z-score: %.2f\n", s.Zscore)

	if note := strings.TrimSpace(s.LLMNote); note != "" {
		fmt.Fprintf(&b, "Note: %s\n", EscapeHTML(note))
	}

	if tx := strings.TrimSpace(attestTx); tx != "" {
		txEsc := EscapeHTML(tx)
		txHref := fmt.Sprintf("%s/tx/%s", base, txEsc)
		fmt.Fprintf(&b, "Attestation: <a href=\"%s\">%s</a>\n", txHref, txEsc)
	}

	return strings.TrimRight(b.String(), "\n")
}

// formatSmartMoney renders a signal_type 3 (accumulating wallet) alert. Its headline
// is a 0..100 composite score (in Zscore), not a z-score, so the title says "score";
// when the payload decodes it also names the accumulating wallet and the evidence. A
// payload that fails to decode still yields a valid alert.
func formatSmartMoney(s store.Signal, poolLabel, attestTx, mantleScanBase string) string {
	base := strings.TrimRight(mantleScanBase, "/")

	label := EscapeHTML(poolLabel)
	pool := EscapeHTML(s.Pool)

	// Decode the actor + accumulation evidence (best-effort): a decode failure
	// leaves these zero/empty and the alert simply omits the wallet line.
	var p struct {
		Actor         string  `json:"actor"`
		AccumSwaps    int     `json:"accum_swaps"`
		DistinctPools int     `json:"distinct_pools"`
		SizeMultiple  float64 `json:"size_multiple"`
	}
	if len(s.Payload) > 0 {
		_ = json.Unmarshal(s.Payload, &p) // best-effort; alert renders without it on error
	}

	var b strings.Builder

	// Title: composite score (not a z-score) so it is not mistaken for a flow anomaly.
	fmt.Fprintf(&b, "🛰 <b>smart money</b> score=%.0f\n", s.Zscore)

	poolHref := fmt.Sprintf("%s/address/%s", base, pool)
	fmt.Fprintf(&b, "Pool: %s (<a href=\"%s\">%s</a>)\n", label, poolHref, pool)

	// Wallet: the accumulating actor (linked) plus evidence, when the payload had one.
	if p.Actor != "" {
		actorEsc := EscapeHTML(p.Actor)
		actorHref := fmt.Sprintf("%s/address/%s", base, actorEsc)
		fmt.Fprintf(&b, "Wallet: <a href=\"%s\">%s</a>\n", actorHref, actorEsc)
		fmt.Fprintf(&b, "Accumulation: %d large swaps across %d pool(s); latest %.1fx pool median\n",
			p.AccumSwaps, p.DistinctPools, p.SizeMultiple)
	}

	// Score on its own line (mirrors the Z-score line for types 1/2).
	fmt.Fprintf(&b, "Score: %.0f\n", s.Zscore)

	if note := strings.TrimSpace(s.LLMNote); note != "" {
		fmt.Fprintf(&b, "Note: %s\n", EscapeHTML(note))
	}

	if tx := strings.TrimSpace(attestTx); tx != "" {
		txEsc := EscapeHTML(tx)
		txHref := fmt.Sprintf("%s/tx/%s", base, txEsc)
		fmt.Fprintf(&b, "Attestation: <a href=\"%s\">%s</a>\n", txHref, txEsc)
	}

	return strings.TrimRight(b.String(), "\n")
}

// formatLending renders an Aave V3 lending alert (type 5 liquidation / 6 big borrow).
// Headline is the event kind plus the display-only USD size (omitted for non-stable
// assets); the body names the subject and the actor, each linked. No z-score wording. A
// payload that fails to decode still yields a valid alert.
func formatLending(s store.Signal, attestTx, mantleScanBase string) string {
	base := strings.TrimRight(mantleScanBase, "/")

	// Best-effort decode of the lending payload for the parties.
	var p struct {
		Liquidator string `json:"liquidator"`
		User       string `json:"user"`
		OnBehalfOf string `json:"on_behalf_of"`
	}
	if len(s.Payload) > 0 {
		_ = json.Unmarshal(s.Payload, &p) // best-effort; alert renders without it on error
	}

	isLiquidation := s.SignalType == liquidationSignalType
	kind := "big borrow"
	emoji := "💰"
	if isLiquidation {
		kind = "liquidation"
		emoji = "⚠️"
	}

	// USD size is display-only.
	size := ""
	if s.SizeUSD != nil {
		size = fmt.Sprintf(" ~$%s", EscapeHTML(s.SizeUSD.String()))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>%s</b>%s\n", emoji, kind, size)

	// Subject: the liquidated borrower (type 5) or the borrowed reserve (type 6), linked.
	subjectLabel := "Reserve"
	if isLiquidation {
		subjectLabel = "Borrower"
	}
	subj := EscapeHTML(s.Pool)
	subjHref := fmt.Sprintf("%s/address/%s", base, subj)
	fmt.Fprintf(&b, "%s: <a href=\"%s\">%s</a>\n", subjectLabel, subjHref, subj)

	// Actor: liquidator (type 5) or borrower (type 6), linked. Prefer the signal's
	// actor column (resolved tx.origin), falling back to the payload party.
	actor := s.Actor
	if actor == "" {
		if isLiquidation {
			actor = p.Liquidator
		} else {
			actor = p.OnBehalfOf
		}
	}
	if actor != "" {
		actorLabel := "Liquidator"
		if !isLiquidation {
			actorLabel = "Borrower"
		}
		actorEsc := EscapeHTML(actor)
		actorHref := fmt.Sprintf("%s/address/%s", base, actorEsc)
		fmt.Fprintf(&b, "%s: <a href=\"%s\">%s</a>\n", actorLabel, actorHref, actorEsc)
	}

	if note := strings.TrimSpace(s.LLMNote); note != "" {
		fmt.Fprintf(&b, "Note: %s\n", EscapeHTML(note))
	}

	if tx := strings.TrimSpace(attestTx); tx != "" {
		txEsc := EscapeHTML(tx)
		txHref := fmt.Sprintf("%s/tx/%s", base, txEsc)
		fmt.Fprintf(&b, "Attestation: <a href=\"%s\">%s</a>\n", txHref, txEsc)
	}

	return strings.TrimRight(b.String(), "\n")
}

// formatLSTFlow renders an LST-flow alert (type 4: a large mETH/cmETH transfer).
// Headline is the classified direction (mint/burn/move) plus the whole-token amount and
// symbol (no USD); the body names the LST token and the resolved actor, each linked. A
// payload that fails to decode still yields a valid alert.
func formatLSTFlow(s store.Signal, attestTx, mantleScanBase string) string {
	base := strings.TrimRight(mantleScanBase, "/")

	// Best-effort decode of the LST-flow payload for the symbol/direction/amount.
	var p struct {
		Symbol    string `json:"symbol"`
		Direction string `json:"direction"`
		ValueLST  string `json:"value_lst"`
	}
	if len(s.Payload) > 0 {
		_ = json.Unmarshal(s.Payload, &p) // best-effort; alert renders without it on error
	}

	// Direction -> headline word + emoji. mint = inflow, burn = outflow, move = OTC.
	kind := "moved"
	emoji := "🔁"
	switch p.Direction {
	case "mint":
		kind = "minted in"
		emoji = "🟢"
	case "burn":
		kind = "redeemed out"
		emoji = "🔴"
	}

	sym := p.Symbol
	if sym == "" {
		sym = "LST"
	}
	// Amount headline (whole tokens) when present.
	amount := ""
	if p.ValueLST != "" {
		amount = fmt.Sprintf(" %s %s", EscapeHTML(p.ValueLST), EscapeHTML(sym))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>LST %s</b>%s\n", emoji, kind, amount)

	// Token (subject): the LST contract, linked.
	token := EscapeHTML(s.Pool)
	tokenHref := fmt.Sprintf("%s/address/%s", base, token)
	fmt.Fprintf(&b, "Token: %s (<a href=\"%s\">%s</a>)\n", EscapeHTML(sym), tokenHref, token)

	// Actor: the resolved tx.origin (or the transfer counterparty fallback), linked.
	if actor := strings.TrimSpace(s.Actor); actor != "" {
		actorEsc := EscapeHTML(actor)
		actorHref := fmt.Sprintf("%s/address/%s", base, actorEsc)
		fmt.Fprintf(&b, "Actor: <a href=\"%s\">%s</a>\n", actorHref, actorEsc)
	}

	if note := strings.TrimSpace(s.LLMNote); note != "" {
		fmt.Fprintf(&b, "Note: %s\n", EscapeHTML(note))
	}

	if tx := strings.TrimSpace(attestTx); tx != "" {
		txEsc := EscapeHTML(tx)
		txHref := fmt.Sprintf("%s/tx/%s", base, txEsc)
		fmt.Fprintf(&b, "Attestation: <a href=\"%s\">%s</a>\n", txHref, txEsc)
	}

	return strings.TrimRight(b.String(), "\n")
}

// formatDepeg renders a depeg alert (type 7: a stablecoin off its $1 peg with a
// contagion map). Headline is the stablecoin + bps off peg; the body shows the implied
// price, the depegging token (linked), and the at-risk pools. No z-score, no USD. A
// payload that fails to decode still yields a valid alert.
func formatDepeg(s store.Signal, attestTx, mantleScanBase string) string {
	base := strings.TrimRight(mantleScanBase, "/")

	// Best-effort decode of the depeg payload for the symbol/price/bps/contagion.
	var p struct {
		TokenSymbol  string `json:"token_symbol"`
		ImpliedPrice string `json:"implied_price"`
		DeviationBps int    `json:"deviation_bps"`
		Affected     []struct {
			Address string `json:"address"`
			Label   string `json:"label"`
		} `json:"affected_pools"`
	}
	if len(s.Payload) > 0 {
		_ = json.Unmarshal(s.Payload, &p) // best-effort; alert renders without it on error
	}

	sym := p.TokenSymbol
	if sym == "" {
		sym = "stablecoin"
	}

	var b strings.Builder
	// Title: depeg + the stablecoin + bps off peg so the alert is scannable.
	fmt.Fprintf(&b, "⚠️ <b>DEPEG</b> %s %d bps off $1\n", EscapeHTML(sym), p.DeviationBps)

	// Implied price comes from swap reserves, not an oracle.
	if p.ImpliedPrice != "" {
		fmt.Fprintf(&b, "Implied price: %s (peg = 1.0)\n", EscapeHTML(p.ImpliedPrice))
	}

	// Token (subject): the depegging stablecoin contract, linked to its address page.
	token := EscapeHTML(s.Pool)
	tokenHref := fmt.Sprintf("%s/address/%s", base, token)
	fmt.Fprintf(&b, "Token: %s (<a href=\"%s\">%s</a>)\n", EscapeHTML(sym), tokenHref, token)

	// Contagion: the at-risk pools holding the token, each label + address linked.
	if len(p.Affected) > 0 {
		fmt.Fprintf(&b, "At-risk pools (%d):\n", len(p.Affected))
		for _, ap := range p.Affected {
			label := ap.Label
			if label == "" {
				label = ap.Address
			}
			apHref := fmt.Sprintf("%s/address/%s", base, EscapeHTML(ap.Address))
			fmt.Fprintf(&b, "  • %s (<a href=\"%s\">pool</a>)\n", EscapeHTML(label), apHref)
		}
	} else {
		fmt.Fprintf(&b, "At-risk pools: none tracked\n")
	}

	if note := strings.TrimSpace(s.LLMNote); note != "" {
		fmt.Fprintf(&b, "Note: %s\n", EscapeHTML(note))
	}

	if tx := strings.TrimSpace(attestTx); tx != "" {
		txEsc := EscapeHTML(tx)
		txHref := fmt.Sprintf("%s/tx/%s", base, txEsc)
		fmt.Fprintf(&b, "Attestation: <a href=\"%s\">%s</a>\n", txHref, txEsc)
	}

	return strings.TrimRight(b.String(), "\n")
}

// Dispatch formats the signal and sends it via the Telegram client, returning the
// sent message's id so the caller can persist it (store.UpdateSignalTGMessageID) and
// later edit the analyst note into the same message. Any send error is wrapped and
// propagated with an empty id. FormatSignal is reused for that later edit, so the two
// renders match apart from the added Note line.
func Dispatch(ctx context.Context, tg *Telegram, s store.Signal, poolLabel, attestTx, mantleScanBase string) (string, error) {
	msgID, err := tg.Send(ctx, FormatSignal(s, poolLabel, attestTx, mantleScanBase))
	if err != nil {
		return "", fmt.Errorf("deliver: dispatch signal %d for pool %s: %w", s.ID, s.Pool, err)
	}
	return msgID, nil
}
