package card

import (
	"bytes"
	"testing"
)

// sample returns a representative Data for one signal family.
func sample(kind, typeLabel, headline string) Data {
	return Data{
		Kind:      kind,
		TypeLabel: typeLabel,
		SigID:     "SIG #142",
		Headline:  headline,
		Sub: []Run{
			{Text: "Wallet ", Tone: ToneSub},
			{Text: "0x8d58…6e10", Bold: true},
			{Text: " · one outsized trade", Tone: ToneFaint},
		},
		Specs: []Spec{
			{Label: "NOTIONAL", Value: "$48.2K"},
			{Label: "VS POOL MEDIAN", Value: "18×", Hot: true},
			{Label: "VENUE", Value: "MERCHANT MOE"},
		},
		AttestTxShort: "0x9c8d…0e1f",
		Timestamp:     "12 JUN 2026 · 14:31 UTC",
	}
}

// Render must produce a valid PNG for every signal family, including the unknown-kind
// fallback color path.
func TestRender_AllKinds(t *testing.T) {
	cases := []struct{ kind, label, headline string }{
		{"flow", "VOLUME SPIKE", "USDT / USDe"},
		{"whale", "WHALE SWAP", "USDe / WMNT"},
		{"smart", "SMART MONEY", "USDC / USDe"},
		{"lst", "LST FLOW", "mETH"},
		{"liq", "LIQUIDATION", "AAVE V3"},
		{"borrow", "BIG BORROW", "AAVE V3"},
		{"depeg", "DEPEG ALERT", "USDe"},
		{"unknown", "OTHER", "SIGNAL"},
	}
	for _, c := range cases {
		png, err := Render(sample(c.kind, c.label, c.headline))
		if err != nil {
			t.Fatalf("Render(%s): %v", c.kind, err)
		}
		if !bytes.HasPrefix(png, []byte("\x89PNG\r\n\x1a\n")) {
			t.Errorf("Render(%s): output is not a PNG (%d bytes)", c.kind, len(png))
		}
	}
}

// A long headline must shrink to fit rather than overflow or fail.
func TestRender_LongHeadlineShrinks(t *testing.T) {
	d := sample("whale", "WHALE SWAP", "VERYLONGTOKEN / ANOTHERLONGTOKEN")
	if _, err := Render(d); err != nil {
		t.Fatalf("Render long headline: %v", err)
	}
}

// Optional fields may all be absent: the renderer degrades (pending attestation, no
// sig id, no timestamp, no specs) instead of failing.
func TestRender_MinimalData(t *testing.T) {
	png, err := Render(Data{Kind: "flow", TypeLabel: "VOLUME SPIKE", Headline: "0x1234…abcd"})
	if err != nil {
		t.Fatalf("Render minimal: %v", err)
	}
	if len(png) == 0 {
		t.Fatal("Render minimal returned no bytes")
	}
}

// Rendering is deterministic (seeded grain, no clocks), so two renders of the same
// data are byte-identical. This pins down accidental nondeterminism, which would break
// visual comparisons across runs.
func TestRender_Deterministic(t *testing.T) {
	d := sample("smart", "SMART MONEY", "USDC / USDe")
	a, err := Render(d)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Render(d)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Error("two renders of identical data differ")
	}
}
