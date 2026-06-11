package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"ocr/internal/attest"
	"ocr/internal/store"
)

// These hermetic tests pin the DTO mapping, the load-bearing part of the public
// shape: nullable fields become real JSON nulls, the on-chain score matches the
// attestor's formula, explorer links are built correctly, and no internal-only
// field leaks. No DB, no HTTP.

const testExplorer = "https://mantlescan.xyz"

func mustLabel(addr string) string {
	if addr == "0xpool" {
		return "USDC/USDe"
	}
	return ""
}

// TestSignalToDTO_FlowMinimal: a flow signal with no actor/note/attest/outcome
// serializes those as JSON null (pointers), and the always-present fields are set.
func TestSignalToDTO_FlowMinimal(t *testing.T) {
	created := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	bucket := time.Date(2026, 6, 6, 4, 30, 0, 0, time.UTC)
	row := store.SignalRow{Signal: store.Signal{
		ID: 60, CreatedAt: created, BucketTS: bucket, Pool: "0xpool",
		SignalType: 1, Metric: "vol1", Zscore: 4.1012, Status: "submitted",
		// actor/note/attest empty; no outcome.
		TGMessageID: "should-not-leak-123",
	}}
	dto := signalToDTO(row, mustLabel, testExplorer)

	if dto.ID != 60 || dto.Type != 1 || dto.TypeName != "flow_anomaly" {
		t.Fatalf("header wrong: %+v", dto)
	}
	if dto.PoolLabel != "USDC/USDe" {
		t.Fatalf("pool_label = %q, want USDC/USDe", dto.PoolLabel)
	}
	if dto.Actor != nil || dto.Note != nil || dto.AttestTx != nil || dto.AttestURL != nil || dto.Outcome != nil {
		t.Fatalf("expected nil pointers for missing fields, got %+v", dto)
	}
	if dto.BucketTS == nil || *dto.BucketTS != "2026-06-06T04:30:00Z" {
		t.Fatalf("bucket_ts wrong: %v", dto.BucketTS)
	}
	if dto.Score != 410 { // round(4.1012*100)
		t.Fatalf("score = %d, want 410", dto.Score)
	}

	// Round-trip through JSON and assert the null-ness + absence of internal fields.
	b, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"actor", "note", "attest_tx", "attest_url", "outcome"} {
		if m[k] != nil {
			t.Fatalf("field %q should be JSON null, got %v", k, m[k])
		}
	}
	if _, leaked := m["tg_message_id"]; leaked {
		t.Fatal("tg_message_id must never be serialized")
	}
	if _, leaked := m["event_ref"]; leaked {
		t.Fatal("event_ref must not be serialized in the lite DTO")
	}
}

// TestSignalToDTO_SmartMoneyWithOutcome: a type-3 signal carries actor + URLs and
// an embedded graded outcome with its own attestation link.
func TestSignalToDTO_SmartMoneyWithOutcome(t *testing.T) {
	grade := 82
	letter := "B"
	status := "confirmed"
	outTx := "0xoutcometx"
	row := store.SignalRow{
		Signal: store.Signal{
			ID: 128, CreatedAt: time.Now().UTC(), Pool: "0xpool",
			SignalType: 3, Metric: "smart_money", Zscore: 77, Status: "alerted",
			Actor: "0xActor", LLMNote: "accumulating", AttestTx: "0xSigTx",
		},
		OutcomeGrade: &grade, OutcomeLetter: &letter, OutcomeStatus: &status, OutcomeAttestTx: &outTx,
	}
	dto := signalToDTO(row, mustLabel, testExplorer)

	if dto.TypeName != "smart_money" {
		t.Fatalf("type_name = %q", dto.TypeName)
	}
	if dto.Actor == nil || *dto.Actor != "0xActor" {
		t.Fatalf("actor = %v", dto.Actor)
	}
	if dto.AttestTx == nil || *dto.AttestTx != "0xSigTx" {
		t.Fatalf("attest_tx = %v", dto.AttestTx)
	}
	if dto.AttestURL == nil || *dto.AttestURL != "https://mantlescan.xyz/tx/0xSigTx" {
		t.Fatalf("attest_url = %v", dto.AttestURL)
	}
	if dto.Outcome == nil {
		t.Fatal("outcome should be present")
	}
	if dto.Outcome.Grade != 82 || dto.Outcome.Letter != "B" || dto.Outcome.Status != "confirmed" {
		t.Fatalf("outcome = %+v", dto.Outcome)
	}
	if dto.Outcome.AttestURL == nil || *dto.Outcome.AttestURL != "https://mantlescan.xyz/tx/0xoutcometx" {
		t.Fatalf("outcome attest_url = %v", dto.Outcome.AttestURL)
	}
	if dto.Score != 7700 {
		t.Fatalf("score = %d, want 7700", dto.Score)
	}
}

// TestOutcomeWithoutAttest: a graded outcome that has no attestation yet keeps
// AttestTx/URL nil (not "").
func TestOutcomeWithoutAttest(t *testing.T) {
	grade, letter, status := 50, "C", "transient"
	empty := ""
	row := store.SignalRow{
		Signal:        store.Signal{ID: 1, CreatedAt: time.Now().UTC(), Pool: "0xpool", SignalType: 1, Metric: "vol0"},
		OutcomeGrade:  &grade,
		OutcomeLetter: &letter,
		OutcomeStatus: &status,
		// attest tx present-but-empty must still yield nil.
		OutcomeAttestTx: &empty,
	}
	dto := signalToDTO(row, mustLabel, testExplorer)
	if dto.Outcome == nil {
		t.Fatal("outcome should be present")
	}
	if dto.Outcome.AttestTx != nil || dto.Outcome.AttestURL != nil {
		t.Fatalf("empty outcome attest should be nil, got tx=%v url=%v", dto.Outcome.AttestTx, dto.Outcome.AttestURL)
	}
}

// TestScoreFromZ pins the display score to the attestor's formula (round, cap to
// uint16 / 100). The API surfaces attest.ScoreFromZ directly (the same value used
// for the on-chain score), so this exercises that function via the int() the DTO uses.
func TestScoreFromZ(t *testing.T) {
	cases := []struct {
		z    float64
		want int
	}{
		{0, 0},
		{4.10, 410},
		{-5.0, 500}, // absolute value
		{77, 7700},
		{151.658, 15166},
		{1000, 65535}, // capped at 655.35*100
	}
	for _, c := range cases {
		if got := int(attest.ScoreFromZ(c.z)); got != c.want {
			t.Fatalf("attest.ScoreFromZ(%g) = %d, want %d", c.z, got, c.want)
		}
	}
}

// TestSignalDetailDTO_EmptyPayload: a detail DTO with no stored payload renders
// "{}" (a valid empty object), never null/invalid.
func TestSignalDetailDTO_EmptyPayload(t *testing.T) {
	row := store.SignalRow{Signal: store.Signal{ID: 5, CreatedAt: time.Now().UTC(), Pool: "0xpool", SignalType: 1, Metric: "vol0", WindowBuckets: 144}}
	d := signalToDetailDTO(row, mustLabel, testExplorer, nil)
	if string(d.Payload) != "{}" {
		t.Fatalf("empty payload = %q, want {}", string(d.Payload))
	}
	if d.WindowBuckets != 144 {
		t.Fatalf("window_buckets = %d", d.WindowBuckets)
	}
	// The whole detail must marshal cleanly with payload as a nested object.
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal detail: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}
	if _, ok := m["payload"].(map[string]any); !ok {
		t.Fatalf("payload should be a JSON object, got %T", m["payload"])
	}
}

// TestSignalTypeName pins the numeric signal_type -> stable string name mapping for
// each family: the LST-flow type (4 = lst_flow), the lending types (5 = liquidation,
// 6 = big_borrow), depeg (7 = depeg), and the "type_N" fallback for an unknown type.
// The frontend keys off these names, so the mapping is part of the public shape.
func TestSignalTypeName(t *testing.T) {
	cases := []struct {
		t    int16
		want string
	}{
		{1, "flow_anomaly"},
		{2, "whale"},
		{3, "smart_money"},
		{4, "lst_flow"},
		{5, "liquidation"},
		{6, "big_borrow"},
		{7, "depeg"},
		{99, "type_99"}, // unknown -> fallback, never silently mislabeled
	}
	for _, c := range cases {
		if got := signalTypeName(c.t); got != c.want {
			t.Errorf("signalTypeName(%d) = %q, want %q", c.t, got, c.want)
		}
	}
}

// TestSignalToDTO_LendingTypeName: a lending signal's DTO carries the lending
// type_name and surfaces its size_usd + tx link (from event_ref), with no actor URL
// dependence breaking when the actor is set.
func TestSignalToDTO_LendingTypeName(t *testing.T) {
	sz := decimal.RequireFromString("50000")
	row := store.SignalRow{Signal: store.Signal{
		ID: 71, CreatedAt: time.Now().UTC(), Pool: "0xborrower",
		SignalType: 5, Metric: "liquidation", Zscore: 7.7, Status: "submitted",
		EventRef: "0xdeadbeef:3", Actor: "0xliquidator", SizeUSD: &sz,
	}}
	dto := signalToDTO(row, mustLabel, testExplorer)
	if dto.Type != 5 || dto.TypeName != "liquidation" {
		t.Fatalf("lending type_name wrong: type=%d name=%q", dto.Type, dto.TypeName)
	}
	if dto.SizeUSD == nil || *dto.SizeUSD != "50000" {
		t.Fatalf("size_usd = %v, want 50000", dto.SizeUSD)
	}
	// event_ref -> tx hash + tx url surfaced; actor url present.
	if dto.TxHash == nil || *dto.TxHash != "0xdeadbeef" {
		t.Fatalf("tx_hash = %v, want 0xdeadbeef", dto.TxHash)
	}
	if dto.ActorURL == nil {
		t.Fatal("lending signal with an actor should have an actor_url")
	}
}

// TestPoolToDTO_SeparatesActivityAndStats: the activity block (our data) and stats
// block (external) are distinct; a nil stats stays JSON null.
func TestPoolToDTO_SeparatesActivityAndStats(t *testing.T) {
	last := time.Date(2026, 6, 8, 9, 0, 0, 0, time.UTC)
	tvl := decimal.RequireFromString("3300225.06")
	row := store.PoolRow{
		Pool: store.Pool{Address: "0xpool", Dex: "agni", Label: "USDe/WMNT",
			Token0: "0xt0", Token1: "0xt1", Dec0: 18, Dec1: 18, Enabled: true},
		Swaps24h: 42, Vol0_24h: decimal.RequireFromString("123.5"),
		Vol1_24h: decimal.RequireFromString("0"), LastBucketTS: &last,
		RecentSignals24h: 3,
		Stats:            &store.PoolStats{Pool: "0xpool", TVLUSD: &tvl, FetchedAt: last, SourceURL: "https://gecko/x"},
	}
	dto := poolToDTO(row, testExplorer, nil)

	if dto.URL != "https://mantlescan.xyz/address/0xpool" {
		t.Fatalf("pool url = %q", dto.URL)
	}
	if dto.Activity.Swaps24h != 42 || dto.Activity.Vol0_24h != "123.5" {
		t.Fatalf("activity wrong: %+v", dto.Activity)
	}
	if dto.Activity.LastBucketTS == nil || *dto.Activity.LastBucketTS != "2026-06-08T09:00:00Z" {
		t.Fatalf("last_bucket_ts = %v", dto.Activity.LastBucketTS)
	}
	if dto.Stats == nil || dto.Stats.TVLUSD == nil || *dto.Stats.TVLUSD != "3300225.06" {
		t.Fatalf("stats tvl wrong: %+v", dto.Stats)
	}
	// price absent -> nil.
	if dto.Stats.PriceUSD != nil {
		t.Fatalf("price should be nil, got %v", dto.Stats.PriceUSD)
	}

	// nil stats -> JSON null.
	row.Stats = nil
	dto2 := poolToDTO(row, testExplorer, nil)
	if dto2.Stats != nil {
		t.Fatalf("nil stats should map to nil DTO, got %+v", dto2.Stats)
	}
}

// TestActorToDTO maps an actor row including the explorer link and grades rollup.
func TestActorToDTO(t *testing.T) {
	a := store.ActorRow{
		Actor: "0xactor", TotalSignals: 6, LargeSwaps24h: 4, PoolsTouched24h: 2,
		LatestScore: 77, BestScore: 88, GradesCount: 2, GradesAvg: 75.5,
		FirstSeen: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		LastSeen:  time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC),
	}
	dto := actorToDTO(a, testExplorer)
	if dto.ActorURL != "https://mantlescan.xyz/address/0xactor" {
		t.Fatalf("actor_url = %q", dto.ActorURL)
	}
	if dto.Grades.Count != 2 || dto.Grades.Avg != 75.5 {
		t.Fatalf("grades = %+v", dto.Grades)
	}
	if dto.FirstSeen != "2026-06-01T00:00:00Z" || dto.LastSeen != "2026-06-08T00:00:00Z" {
		t.Fatalf("seen times wrong: %s / %s", dto.FirstSeen, dto.LastSeen)
	}
}

// TestStatsToDTO maps the summary and stamps updated_at; absent first-signal -> null.
func TestStatsToDTO(t *testing.T) {
	now := time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC)
	sum := store.StatsSummary{
		TotalSignals: 60, ByTypeFlow: 60,
		SignalAttestations: 60, OutcomesTotal: 60,
		GradeA: 18, GradeB: 28, GradeC: 3, GradeD: 7, GradeF: 4, AvgGrade: 73.6,
		Last24hSignals: 0, PoolsTracked: 6,
		Last24hOutcomesTotal: 12, Last24hGradeA: 5, Last24hGradeB: 4, Last24hGradeC: 1,
		Last24hGradeD: 1, Last24hGradeF: 1, Last24hAvgGrade: 71.2,
		FirstSignalAt: time.Date(2026, 6, 6, 11, 24, 38, 0, time.UTC),
	}
	dto := statsToDTO(sum, now)
	if dto.TotalSignals != 60 || dto.ByType.Flow != 60 || dto.GradeDistribution.A != 18 {
		t.Fatalf("stats wrong: %+v", dto)
	}
	if dto.FirstSignalAt == nil || *dto.FirstSignalAt != "2026-06-06T11:24:38Z" {
		t.Fatalf("first_signal_at = %v", dto.FirstSignalAt)
	}
	if dto.UpdatedAt != "2026-06-08T10:00:00Z" {
		t.Fatalf("updated_at = %q", dto.UpdatedAt)
	}
	if dto.Last24h.TopActor != nil {
		t.Fatalf("top_actor should be nil, got %v", dto.Last24h.TopActor)
	}
	// 24h grade breakdown maps into last_24h (the momentum view).
	if dto.Last24h.OutcomesTotal != 12 || dto.Last24h.GradeDistribution.A != 5 ||
		dto.Last24h.GradeDistribution.F != 1 || dto.Last24h.AvgGrade != 71.2 {
		t.Fatalf("last_24h grades wrong: %+v", dto.Last24h)
	}

	// no signals -> first_signal_at null.
	dto2 := statsToDTO(store.StatsSummary{}, now)
	if dto2.FirstSignalAt != nil {
		t.Fatalf("first_signal_at should be nil for empty, got %v", dto2.FirstSignalAt)
	}
}

// TestTxURLNilWhenEmpty: no base or no hash -> nil link.
func TestTxURLNilWhenEmpty(t *testing.T) {
	if txURL("", "0xabc") != nil {
		t.Fatal("no base should yield nil")
	}
	if txURL("https://x", "") != nil {
		t.Fatal("no hash should yield nil")
	}
	got := txURL("https://x", "0xabc")
	if got == nil || *got != "https://x/tx/0xabc" {
		t.Fatalf("link = %v", got)
	}
}

// TestSignalsToDTOsNeverNil: an empty input yields a non-nil empty slice (JSON []).
func TestSignalsToDTOsNeverNil(t *testing.T) {
	out := signalsToDTOs(nil, mustLabel, testExplorer)
	if out == nil {
		t.Fatal("expected non-nil empty slice")
	}
	b, _ := json.Marshal(out)
	if string(b) != "[]" {
		t.Fatalf("empty slice should marshal to [], got %s", b)
	}
}
