package enrich

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Noop is the disabled enricher: it returns an empty note and never errors.
func TestNoop(t *testing.T) {
	note, err := Noop{}.Analyze(context.Background(), SignalContext{Pool: "0xabc"})
	if note != "" || err != nil {
		t.Errorf("Noop.Analyze = (%q, %v), want (\"\", nil)", note, err)
	}
}

// NewProvider returns a Noop unless both baseURL and apiKey are set.
func TestNewProvider_Selection(t *testing.T) {
	cases := []struct {
		name         string
		baseURL, key string
		wantNoop     bool
	}{
		{"both-empty", "", "", true},
		{"missing-key", "https://api.example/v1", "", true},
		{"missing-url", "", "sk-123", true},
		{"both-set", "https://api.example/v1", "sk-123", false},
	}
	for _, c := range cases {
		p := NewProvider(c.baseURL, c.key, "model")
		_, isNoop := p.(Noop)
		if isNoop != c.wantNoop {
			t.Errorf("%s: NewProvider noop=%v, want %v (type %T)", c.name, isNoop, c.wantNoop, p)
		}
	}
}

// Every non-2xx / transport / decode / empty-content path must return ("", err);
// enrichment is best-effort and must never panic or block attestation.
func TestOpenAIProvider_FailureModes(t *testing.T) {
	t.Run("http-500", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
		}))
		defer srv.Close()
		note, err := NewProvider(srv.URL, "k", "m").Analyze(context.Background(), SignalContext{})
		if err == nil || note != "" {
			t.Errorf("500: got (%q, %v), want (\"\", err)", note, err)
		}
	})

	t.Run("transport-error", func(t *testing.T) {
		// Closed port -> Do() fails at the transport layer.
		note, err := NewProvider("http://127.0.0.1:1", "k", "m").Analyze(context.Background(), SignalContext{})
		if err == nil || note != "" {
			t.Errorf("transport: got (%q, %v), want (\"\", err)", note, err)
		}
	})

	t.Run("malformed-json-200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{not json`))
		}))
		defer srv.Close()
		_, err := NewProvider(srv.URL, "k", "m").Analyze(context.Background(), SignalContext{})
		if err == nil {
			t.Errorf("malformed 200: want error")
		}
	})

	t.Run("no-choices", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"choices":[]}`))
		}))
		defer srv.Close()
		_, err := NewProvider(srv.URL, "k", "m").Analyze(context.Background(), SignalContext{})
		if err == nil {
			t.Errorf("no choices: want error")
		}
	})

	t.Run("empty-content", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"  \n "}}]}`))
		}))
		defer srv.Close()
		note, err := NewProvider(srv.URL, "k", "m").Analyze(context.Background(), SignalContext{})
		if err == nil || note != "" {
			t.Errorf("empty content: got (%q, %v), want (\"\", err)", note, err)
		}
	})

	t.Run("nil-http-client", func(t *testing.T) {
		// Directly constructed provider with no HTTP client must error, not panic.
		p := &OpenAIProvider{BaseURL: "https://x", APIKey: "k", Model: "m"}
		if _, err := p.Analyze(context.Background(), SignalContext{}); err == nil {
			t.Errorf("nil HTTP client: want error")
		}
	})

	t.Run("cancelled-context", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
		defer srv.Close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := NewProvider(srv.URL, "k", "m").Analyze(ctx, SignalContext{}); err == nil {
			t.Errorf("cancelled context: want error")
		}
	})
}

// FallbackNote must always return a non-empty, sensible note built from the
// signal's own numbers; it is the guaranteed backstop when the LLM is
// unavailable, so no signal ever ships with a blank note.
func TestFallbackNote(t *testing.T) {
	// Bucket signal: quotes the metric, label, latest/median ratio, and z-score.
	t.Run("bucket-signal", func(t *testing.T) {
		note := FallbackNote(SignalContext{
			Pool:        "0xbcf99c834e65e8a58090e20edc058279317865bd",
			PoolLabel:   "USDC/USDe",
			Metric:      "vol0",
			Zscore:      1031.0,
			LatestValue: 5000,
			Median:      100, // ratio 50.0x
		})
		if note == "" {
			t.Fatal("bucket fallback note is empty")
		}
		for _, want := range []string{"USDC/USDe", "vol0", "50.0x", "1031.00", "LLM unavailable"} {
			if !strings.Contains(note, want) {
				t.Errorf("bucket note missing %q: %s", want, note)
			}
		}
	})

	// Whale signal: phrased per-transaction ("whale swap"), still non-empty.
	t.Run("whale-signal", func(t *testing.T) {
		note := FallbackNote(SignalContext{
			Pool:        "0xeafc4d6d4c3391cd4fc10c85d2f5f972d58c0dd5",
			PoolLabel:   "USDe/WMNT",
			Metric:      "whale_swap",
			Zscore:      8.5,
			LatestValue: 2750,
			Median:      100, // 27.5x
		})
		if note == "" {
			t.Fatal("whale fallback note is empty")
		}
		for _, want := range []string{"USDe/WMNT", "whale", "27.5x", "8.50"} {
			if !strings.Contains(note, want) {
				t.Errorf("whale note missing %q: %s", want, note)
			}
		}
	})

	// Smart-money signal: phrased as wallet accumulation, names the actor, the
	// large-swap count, the pool breadth, the window, and the latest size multiple.
	// It must not describe a z-score or a 5-minute window (it is not a flow anomaly).
	t.Run("smart-money-signal", func(t *testing.T) {
		note := FallbackNote(SignalContext{
			Pool:          "0xeafc4d6d4c3391cd4fc10c85d2f5f972d58c0dd5",
			PoolLabel:     "USDe/WMNT",
			Metric:        "smart_money",
			IsSmartMoney:  true,
			Actor:         "0x00000000000000000000000000000000000000aa",
			AccumSwaps:    4,
			DistinctPools: 2,
			SizeMultiple:  7.5,
			WindowHours:   24,
		})
		if note == "" {
			t.Fatal("smart-money fallback note is empty")
		}
		for _, want := range []string{"0x00000000000000000000000000000000000000aa", "accumulating", "4 large swap", "2 pool", "24h", "7.5x"} {
			if !strings.Contains(note, want) {
				t.Errorf("smart-money note missing %q: %s", want, note)
			}
		}
		// It is accumulation, not a 5-minute z-score: the flow wording must be absent.
		for _, banned := range []string{"z=", "last 5m", "baseline median"} {
			if strings.Contains(note, banned) {
				t.Errorf("smart-money note leaked flow-anomaly wording %q: %s", banned, note)
			}
		}
	})

	// Smart-money with no size multiple (e.g. an exactly-at-median edge) still
	// renders a non-empty note and simply omits the size clause.
	t.Run("smart-money-no-size-multiple", func(t *testing.T) {
		note := FallbackNote(SignalContext{
			IsSmartMoney:  true,
			PoolLabel:     "USDe/WMNT",
			Actor:         "0x00000000000000000000000000000000000000bb",
			AccumSwaps:    2,
			DistinctPools: 1,
			WindowHours:   24,
		})
		if note == "" {
			t.Fatal("smart-money (no multiple) fallback note is empty")
		}
		if strings.Contains(note, "Inf") || strings.Contains(note, "NaN") {
			t.Errorf("smart-money note leaked a non-finite value: %s", note)
		}
	})

	// Zero baseline median: no ratio to quote, but the note is still non-empty
	// and never contains a divide-by-zero artifact ("Inf"/"NaN").
	t.Run("zero-median-no-divide-by-zero", func(t *testing.T) {
		note := FallbackNote(SignalContext{
			Pool:   "0xabc",
			Metric: "netflow0",
			Zscore: 5.0,
			Median: 0,
		})
		if note == "" {
			t.Fatal("zero-median fallback note is empty")
		}
		if strings.Contains(note, "Inf") || strings.Contains(note, "NaN") {
			t.Errorf("zero-median note leaked a non-finite ratio: %s", note)
		}
	})

	// No label and no address: the note still renders a non-empty pool token.
	t.Run("no-label-falls-back-to-placeholder", func(t *testing.T) {
		note := FallbackNote(SignalContext{Metric: "swap_count", Zscore: 4.2, Median: 10, LatestValue: 80})
		if note == "" {
			t.Fatal("no-label fallback note is empty")
		}
		if !strings.Contains(note, "unknown pool") {
			t.Errorf("expected an unknown-pool placeholder, got: %s", note)
		}
	})
}

// Happy path: correct endpoint, auth header, trailing-slash trimming, and the
// returned note is the trimmed model content.
func TestOpenAIProvider_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("auth = %q, want %q", got, "Bearer k")
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"  Volume spiked. Arb or rush. medium  "}}]}`))
	}))
	defer srv.Close()

	// Trailing slash on BaseURL must be trimmed (no //chat/completions).
	note, err := NewProvider(srv.URL+"/", "k", "m").Analyze(context.Background(), SignalContext{Pool: "0xabc", Metric: "vol0"})
	if err != nil {
		t.Fatalf("happy path error: %v", err)
	}
	if note != "Volume spiked. Arb or rush. medium" {
		t.Errorf("note = %q (want trimmed content)", note)
	}
}

// A smart-money SignalContext routes to the accumulation system prompt + user
// prompt: the request must carry the actor, the accumulation evidence, and the
// "score (0-100, not a z-score)" framing, and must not frame it as a 5-minute
// flow window. This checks the smart-money note framing end-to-end through Analyze
// (the prompt the model actually receives).
func TestOpenAIProvider_SmartMoneyPrompt(t *testing.T) {
	var gotSystem, gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		for _, m := range req.Messages {
			switch m.Role {
			case "system":
				gotSystem = m.Content
			case "user":
				gotUser = m.Content
			}
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Accumulating. Informed or MM. medium"}}]}`))
	}))
	defer srv.Close()

	in := SignalContext{
		Pool:          "0xeafc4d6d4c3391cd4fc10c85d2f5f972d58c0dd5",
		PoolLabel:     "USDe/WMNT",
		Metric:        "smart_money",
		IsSmartMoney:  true,
		Actor:         "0x00000000000000000000000000000000000000aa",
		AccumSwaps:    4,
		DistinctPools: 2,
		SizeMultiple:  7.5,
		WindowHours:   24,
		Zscore:        56, // the composite score, carried in Zscore
		BucketTime:    "2026-06-07T00:00:00Z",
	}
	if _, err := NewProvider(srv.URL, "k", "m").Analyze(context.Background(), in); err != nil {
		t.Fatalf("smart-money analyze error: %v", err)
	}

	// System prompt must be the smart-money framing (accumulating wallet), not flow.
	if !strings.Contains(gotSystem, "smart-money") || !strings.Contains(gotSystem, "ACCUMULATING") {
		t.Errorf("system prompt is not the smart-money framing: %s", gotSystem)
	}
	// User prompt must carry the actor + accumulation evidence and the score framing.
	for _, want := range []string{"0x00000000000000000000000000000000000000aa", "Large swaps in window: 4", "Distinct pools touched: 2", "24 hours", "not a z-score", "7.5x"} {
		if !strings.Contains(gotUser, want) {
			t.Errorf("smart-money user prompt missing %q: %s", want, gotUser)
		}
	}
	// And must not frame it as a 5-minute flow window or a robust z-score line.
	for _, banned := range []string{"Window: 5 minutes", "Robust z-score"} {
		if strings.Contains(gotUser, banned) {
			t.Errorf("smart-money user prompt leaked flow wording %q: %s", banned, gotUser)
		}
	}
}

// A flow (type 1) / whale (type 2) SignalContext is unchanged by the smart-money
// branch: it must still receive the original flow system prompt and the
// "Window: 5 minutes" / "Robust z-score" user prompt. Confirms the smart-money
// branch leaves the flow path intact.
func TestOpenAIProvider_FlowPromptUnchanged(t *testing.T) {
	var gotSystem, gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		for _, m := range req.Messages {
			switch m.Role {
			case "system":
				gotSystem = m.Content
			case "user":
				gotUser = m.Content
			}
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok medium"}}]}`))
	}))
	defer srv.Close()

	in := SignalContext{
		Pool:        "0xabc",
		PoolLabel:   "USDC/USDe",
		Metric:      "vol0",
		Zscore:      9.1,
		LatestValue: 5000,
		Median:      100,
		BucketTime:  "2026-06-07T00:00:00Z",
		// IsSmartMoney left false.
	}
	if _, err := NewProvider(srv.URL, "k", "m").Analyze(context.Background(), in); err != nil {
		t.Fatalf("flow analyze error: %v", err)
	}
	if gotSystem != systemPrompt {
		t.Errorf("flow system prompt changed; want the original systemPrompt, got: %s", gotSystem)
	}
	for _, want := range []string{"Window: 5 minutes", "Robust z-score: 9.10", "vol0"} {
		if !strings.Contains(gotUser, want) {
			t.Errorf("flow user prompt missing %q: %s", want, gotUser)
		}
	}
}

// A liquidation (type 5) SignalContext gets the liquidation system framing and a
// user prompt naming the liquidator/borrower/assets + USD size, with no flow or
// smart-money wording. Confirms the lending branch leaves the other paths intact.
func TestOpenAIProvider_LiquidationPrompt(t *testing.T) {
	var gotSystem, gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		for _, m := range req.Messages {
			switch m.Role {
			case "system":
				gotSystem = m.Content
			case "user":
				gotUser = m.Content
			}
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Liquidation. Bot. medium"}}]}`))
	}))
	defer srv.Close()

	in := SignalContext{
		Metric:          "liquidation",
		IsLending:       true,
		IsLiquidation:   true,
		Liquidator:      "0xliquidator",
		LiquidatedUser:  "0xborrower",
		CollateralAsset: "0xfbtc",
		DebtAsset:       "0xusdt0",
		SizeUSD:         "50000",
		BucketTime:      "2026-06-09T00:00:00Z",
	}
	if _, err := NewProvider(srv.URL, "k", "m").Analyze(context.Background(), in); err != nil {
		t.Fatalf("liquidation analyze error: %v", err)
	}
	if !strings.Contains(gotSystem, "lending analyst") || !strings.Contains(gotSystem, "LIQUIDATION") {
		t.Errorf("system prompt is not the liquidation framing: %s", gotSystem)
	}
	for _, want := range []string{"0xliquidator", "0xborrower", "$50000", "liquidation"} {
		if !strings.Contains(gotUser, want) {
			t.Errorf("liquidation user prompt missing %q: %s", want, gotUser)
		}
	}
	for _, banned := range []string{"Window: 5 minutes", "Robust z-score", "ACCUMULATING"} {
		if strings.Contains(gotUser, banned) || strings.Contains(gotSystem, banned) {
			t.Errorf("liquidation prompt leaked %q", banned)
		}
	}
}

// A big borrow (type 6) gets the borrow system framing and a prompt naming the
// borrower/reserve + USD size.
func TestOpenAIProvider_BigBorrowPrompt(t *testing.T) {
	var gotSystem, gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		for _, m := range req.Messages {
			switch m.Role {
			case "system":
				gotSystem = m.Content
			case "user":
				gotUser = m.Content
			}
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Borrow. Leverage. low"}}]}`))
	}))
	defer srv.Close()

	in := SignalContext{
		Metric:     "big_borrow",
		IsLending:  true,
		Reserve:    "0xusdt0",
		OnBehalfOf: "0xborrowerwallet",
		SizeUSD:    "75000",
		BucketTime: "2026-06-09T00:00:00Z",
	}
	if _, err := NewProvider(srv.URL, "k", "m").Analyze(context.Background(), in); err != nil {
		t.Fatalf("big-borrow analyze error: %v", err)
	}
	if !strings.Contains(gotSystem, "lending analyst") || !strings.Contains(gotSystem, "BORROW") {
		t.Errorf("system prompt is not the big-borrow framing: %s", gotSystem)
	}
	for _, want := range []string{"0xborrowerwallet", "0xusdt0", "$75000", "borrow"} {
		if !strings.Contains(gotUser, want) {
			t.Errorf("big-borrow user prompt missing %q: %s", want, gotUser)
		}
	}
}

// FallbackNote renders a lending finding (liquidation / big borrow) as a discrete
// event with its USD size, naming the parties, never flow or accumulation wording.
func TestFallbackNote_Lending(t *testing.T) {
	// Liquidation: names the liquidator + borrower + USD size.
	liq := FallbackNote(SignalContext{
		Metric:         "liquidation",
		IsLending:      true,
		IsLiquidation:  true,
		Liquidator:     "0xliquidator",
		LiquidatedUser: "0xborrower",
		SizeUSD:        "50000",
	})
	if liq == "" {
		t.Fatal("liquidation fallback note is empty")
	}
	for _, want := range []string{"liquidation", "0xliquidator", "0xborrower", "$50000"} {
		if !strings.Contains(liq, want) {
			t.Errorf("liquidation fallback missing %q: %s", want, liq)
		}
	}
	for _, banned := range []string{"z=", "last 5m", "accumulating"} {
		if strings.Contains(liq, banned) {
			t.Errorf("liquidation fallback leaked %q: %s", banned, liq)
		}
	}

	// Big borrow with no USD size (non-stable) still renders a non-empty note and
	// omits the size clause.
	borrow := FallbackNote(SignalContext{
		Metric:     "big_borrow",
		IsLending:  true,
		OnBehalfOf: "0xborrowerwallet",
		PoolLabel:  "0xfbtc",
		// no SizeUSD
	})
	if borrow == "" {
		t.Fatal("big-borrow fallback note is empty")
	}
	if !strings.Contains(borrow, "borrow") || !strings.Contains(borrow, "0xborrowerwallet") {
		t.Errorf("big-borrow fallback missing the borrow/wallet: %s", borrow)
	}
	if strings.Contains(borrow, "$") {
		t.Errorf("no-size big-borrow fallback should omit the USD clause: %s", borrow)
	}
}

// An LST-flow (type 4) SignalContext gets the LST-flow system framing and a user
// prompt naming the token, direction, and whole-token amount, with no flow,
// smart-money, lending, or USD wording (mETH/cmETH are ETH-priced). Confirms the
// lst_flow branch leaves the other branches intact.
func TestOpenAIProvider_LSTFlowPrompt(t *testing.T) {
	var gotSystem, gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		for _, m := range req.Messages {
			switch m.Role {
			case "system":
				gotSystem = m.Content
			case "user":
				gotUser = m.Content
			}
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"LST minted. Inflow. medium"}}]}`))
	}))
	defer srv.Close()

	in := SignalContext{
		Metric:       "lst_flow",
		IsLSTFlow:    true,
		LSTSymbol:    "mETH",
		LSTDirection: "mint",
		LSTValue:     "100",
		LSTFrom:      "0x0000000000000000000000000000000000000000",
		LSTTo:        "0xrecipient",
		BucketTime:   "2026-06-09T00:00:00Z",
	}
	if _, err := NewProvider(srv.URL, "k", "m").Analyze(context.Background(), in); err != nil {
		t.Fatalf("lst-flow analyze error: %v", err)
	}
	if !strings.Contains(gotSystem, "liquid-staking-token") || !strings.Contains(gotSystem, "mint") {
		t.Errorf("system prompt is not the LST-flow framing: %s", gotSystem)
	}
	for _, want := range []string{"mETH", "mint", "100", "0xrecipient"} {
		if !strings.Contains(gotUser, want) {
			t.Errorf("lst-flow user prompt missing %q: %s", want, gotUser)
		}
	}
	// Must not leak the flow/smart-money framing, nor invent a dollar figure.
	for _, banned := range []string{"Window: 5 minutes", "Robust z-score", "ACCUMULATING", "$"} {
		if strings.Contains(gotUser, banned) {
			t.Errorf("lst-flow user prompt leaked %q: %s", banned, gotUser)
		}
	}
}

// FallbackNote renders an LST-flow finding (mint / burn / move) as a discrete token
// transfer, never flow, accumulation, or USD wording.
func TestFallbackNote_LSTFlow(t *testing.T) {
	cases := []struct {
		dir        string
		wantPhrase string
	}{
		{"mint", "INTO Mantle"},
		{"burn", "OUT of Mantle"},
		{"move", "wallet-to-wallet"},
	}
	for _, c := range cases {
		note := FallbackNote(SignalContext{
			Metric:       "lst_flow",
			IsLSTFlow:    true,
			LSTSymbol:    "mETH",
			LSTDirection: c.dir,
			LSTValue:     "100",
		})
		if note == "" {
			t.Fatalf("%s: lst-flow fallback note is empty", c.dir)
		}
		for _, want := range []string{"LST flow", "mETH", "100", c.wantPhrase} {
			if !strings.Contains(note, want) {
				t.Errorf("%s: lst-flow fallback missing %q: %s", c.dir, want, note)
			}
		}
		// No flow/accumulation/USD wording.
		for _, banned := range []string{"z=", "last 5m", "accumulating", "$"} {
			if strings.Contains(note, banned) {
				t.Errorf("%s: lst-flow fallback leaked %q: %s", c.dir, banned, note)
			}
		}
	}
}

// The depeg branch (type 7) selects the depeg system framing and a user prompt naming
// the depegging stablecoin, the implied price, the bps off peg, and the at-risk pools,
// and never leaks the flow/accumulation framing or invents an oracle/z-score/window.
func TestOpenAIProvider_DepegPrompt(t *testing.T) {
	var gotSystem, gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		for _, m := range req.Messages {
			switch m.Role {
			case "system":
				gotSystem = m.Content
			case "user":
				gotUser = m.Content
			}
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"USDe is below peg. Redemption pressure. medium"}}]}`))
	}))
	defer srv.Close()

	in := SignalContext{
		Metric:           "depeg",
		PoolLabel:        "USDe/USDC",
		IsDepeg:          true,
		DepegSymbol:      "USDe",
		DepegImpliedPx:   "0.971",
		DepegDeviationBp: 290,
		DepegAffected:    "USDC/USDe, USDT/USDe",
		BucketTime:       "2026-06-09T00:00:00Z",
	}
	if _, err := NewProvider(srv.URL, "k", "m").Analyze(context.Background(), in); err != nil {
		t.Fatalf("depeg analyze error: %v", err)
	}
	if !strings.Contains(gotSystem, "stablecoin peg") || !strings.Contains(gotSystem, "contagion") {
		t.Errorf("system prompt is not the depeg framing: %s", gotSystem)
	}
	for _, want := range []string{"USDe", "0.971", "290", "USDC/USDe", "USDT/USDe"} {
		if !strings.Contains(gotUser, want) {
			t.Errorf("depeg user prompt missing %q: %s", want, gotUser)
		}
	}
	// Must not leak the flow/smart-money framing. (The peg reference "$1" is legitimate
	// here, unlike a USD notional, so a bare "$" is not banned; the system prompt is
	// what forbids inventing a dollar volume.)
	for _, banned := range []string{"Window: 5 minutes", "Robust z-score", "ACCUMULATING"} {
		if strings.Contains(gotUser, banned) {
			t.Errorf("depeg user prompt leaked %q: %s", banned, gotUser)
		}
	}
}

// FallbackNote renders a depeg finding as a stablecoin off peg with its at-risk pools,
// never flow, accumulation, or USD-notional wording.
func TestFallbackNote_Depeg(t *testing.T) {
	note := FallbackNote(SignalContext{
		Metric:           "depeg",
		IsDepeg:          true,
		DepegSymbol:      "USDe",
		DepegImpliedPx:   "0.971",
		DepegDeviationBp: 290,
		DepegAffected:    "USDC/USDe, USDT/USDe",
	})
	if note == "" {
		t.Fatal("depeg fallback note is empty")
	}
	for _, want := range []string{"USDe", "off its $1 peg", "0.971", "290 bps", "USDC/USDe"} {
		if !strings.Contains(note, want) {
			t.Errorf("depeg fallback missing %q: %s", want, note)
		}
	}
	// No flow/accumulation wording.
	for _, banned := range []string{"z=", "last 5m", "accumulating"} {
		if strings.Contains(note, banned) {
			t.Errorf("depeg fallback leaked %q: %s", banned, note)
		}
	}
}
