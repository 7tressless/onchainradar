package outcome

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"ocr/internal/store"
)

// These tests cover the pure scorer (ScoreOutcome) and the deterministic
// report-hash path. They are fully hermetic (no DB, no RPC, no wall clock), so
// they exercise the exact grading the Measure loop relies on.

// vbucket builds a bucket carrying only the fields the scorer reads: vol0/vol1 (size
// metrics) and netflow0 (the signed metric for the DC facet). The rest stay zero; the
// tests select metrics explicitly.
func vbucket(vol0, vol1, netflow0 float64) store.Bucket {
	return store.Bucket{
		Pool:     "0xpool",
		TsBucket: time.Unix(0, 0).UTC(),
		Vol0:     decimal.NewFromFloat(vol0),
		Vol1:     decimal.NewFromFloat(vol1),
		Netflow0: decimal.NewFromFloat(netflow0),
	}
}

// repeat returns n copies of b, a tidy way to build a uniform window.
func repeat(b store.Bucket, n int) []store.Bucket {
	out := make([]store.Bucket, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, b)
	}
	return out
}

// assertGradeInvariants checks the universal contract: grade in [0,100] and the
// letter is exactly what letterFor maps the grade to.
func assertGradeInvariants(t *testing.T, c Card) {
	t.Helper()
	if c.Grade < 0 || c.Grade > 100 {
		t.Errorf("grade out of [0,100]: %d", c.Grade)
	}
	if got := letterFor(c.Grade); got != c.Letter {
		t.Errorf("letter %q does not match grade %d (want %q)", c.Letter, c.Grade, got)
	}
}

// A clearly-sustained vol0 signal (post 3x baseline, every window elevated)
// grades at the top of the scale.
func TestScoreOutcome_SustainedGradesHigh(t *testing.T) {
	sig := store.Signal{ID: 1, Pool: "0xpool", SignalType: 1, Metric: "vol0"}
	pre := repeat(vbucket(100, 0, 0), 6)  // pre_median = 100
	post := repeat(vbucket(300, 0, 0), 6) // 3x baseline, sustained

	card, ok := ScoreOutcome(sig, pre, post, 6)
	if !ok {
		t.Fatalf("expected ok=true for a gradeable sustained signal")
	}
	assertGradeInvariants(t, card)

	if card.PreMedian != 100 {
		t.Errorf("pre_median = %v, want 100", card.PreMedian)
	}
	if card.Facets.FollowThrough != 3 {
		t.Errorf("follow_through = %v, want 3", card.Facets.FollowThrough)
	}
	if card.Facets.Persistence != 1 {
		t.Errorf("persistence = %v, want 1", card.Facets.Persistence)
	}
	if card.Grade < gradeA {
		t.Errorf("sustained signal graded %d, expected >= %d (A)", card.Grade, gradeA)
	}
	if card.Letter != "A" {
		t.Errorf("sustained signal letter = %q, want A", card.Letter)
	}
	if card.Status != statusConfirmed {
		t.Errorf("sustained signal status = %q, want %q", card.Status, statusConfirmed)
	}
	// A non-netflow signal must carry no direction facet/sub-score.
	if card.Facets.Direction != nil || card.Subs.DC != nil {
		t.Errorf("non-netflow signal must have nil direction/DC, got dir=%v dc=%v", card.Facets.Direction, card.Subs.DC)
	}
}

// A reverted signal (activity collapses far below baseline) grades at the bottom.
func TestScoreOutcome_RevertedGradesLow(t *testing.T) {
	sig := store.Signal{ID: 2, Pool: "0xpool", SignalType: 1, Metric: "vol0"}
	pre := repeat(vbucket(100, 0, 0), 6) // pre_median = 100
	post := repeat(vbucket(5, 0, 0), 6)  // collapsed to 5% of baseline

	card, ok := ScoreOutcome(sig, pre, post, 6)
	if !ok {
		t.Fatalf("expected ok=true (enough data; just a bad outcome)")
	}
	assertGradeInvariants(t, card)

	if card.Facets.Persistence != 0 {
		t.Errorf("persistence = %v, want 0 (nothing held at baseline)", card.Facets.Persistence)
	}
	if card.Grade >= gradeC {
		t.Errorf("reverted signal graded %d, expected < %d", card.Grade, gradeC)
	}
	if card.Letter != "F" {
		t.Errorf("reverted signal letter = %q, want F", card.Letter)
	}
	if card.Status != statusTransient {
		t.Errorf("reverted signal status = %q, want %q", card.Status, statusTransient)
	}
}

// A netflow signal whose payload carries a signed latest value gets a DC facet,
// and DC is positive when the post-window net flow continued the same direction.
func TestScoreOutcome_NetflowGetsDirectionFacet(t *testing.T) {
	payload, _ := json.Marshal(map[string]float64{"latest": 1000}) // signed +1000
	sig := store.Signal{
		ID: 3, Pool: "0xpool", SignalType: 1, Metric: "netflow0",
		Payload: payload,
	}
	// Measured metric for netflow0 is vol0; pre_median over vol0 = 100.
	pre := repeat(vbucket(100, 0, 0), 6)
	// Post: vol0=200 (sustained size) and netflow0=+500 (same direction as +1000).
	post := repeat(vbucket(200, 0, 500), 6)

	card, ok := ScoreOutcome(sig, pre, post, 6)
	if !ok {
		t.Fatalf("expected ok=true for a gradeable netflow signal")
	}
	assertGradeInvariants(t, card)

	if card.Facets.Direction == nil {
		t.Fatalf("netflow signal must have a non-nil direction facet")
	}
	if card.Subs.DC == nil {
		t.Fatalf("netflow signal must have a non-nil DC sub-score")
	}
	// direction = mean(+500) / 1000 = 0.5 ; DC_sub = clamp(0.5/1*100) = 50.
	if *card.Facets.Direction != 0.5 {
		t.Errorf("direction = %v, want 0.5", *card.Facets.Direction)
	}
	if *card.Subs.DC != 50 {
		t.Errorf("DC sub = %v, want 50", *card.Subs.DC)
	}
}

// A netflow signal with a reversed post-window net flow gets a negative direction
// and a clamped-to-zero DC sub-score (the call's direction did not hold).
func TestScoreOutcome_NetflowReversedDirection(t *testing.T) {
	payload, _ := json.Marshal(map[string]float64{"latest": 1000}) // +1000
	sig := store.Signal{ID: 4, Pool: "0xpool", SignalType: 1, Metric: "netflow0", Payload: payload}
	pre := repeat(vbucket(100, 0, 0), 6)
	post := repeat(vbucket(200, 0, -500), 6) // net flow reversed

	card, ok := ScoreOutcome(sig, pre, post, 6)
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if card.Facets.Direction == nil || *card.Facets.Direction >= 0 {
		t.Errorf("expected negative direction, got %v", card.Facets.Direction)
	}
	if card.Subs.DC == nil || *card.Subs.DC != 0 {
		t.Errorf("expected DC clamped to 0 for reversed direction, got %v", card.Subs.DC)
	}
}

// A netflow signal whose payload.latest is 0 (no usable signed reference) drops
// the DC facet rather than dividing by zero.
func TestScoreOutcome_NetflowZeroLatestDropsDC(t *testing.T) {
	payload, _ := json.Marshal(map[string]float64{"latest": 0})
	sig := store.Signal{ID: 5, Pool: "0xpool", SignalType: 1, Metric: "netflow0", Payload: payload}
	pre := repeat(vbucket(100, 0, 0), 6)
	post := repeat(vbucket(200, 0, 500), 6)

	card, ok := ScoreOutcome(sig, pre, post, 6)
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if card.Facets.Direction != nil || card.Subs.DC != nil {
		t.Errorf("payload.latest==0 must drop DC, got dir=%v dc=%v", card.Facets.Direction, card.Subs.DC)
	}
}

// Fewer than minPreBuckets pre buckets is not gradeable: ok=false.
func TestScoreOutcome_TooFewPreBuckets(t *testing.T) {
	sig := store.Signal{ID: 6, Pool: "0xpool", SignalType: 1, Metric: "vol0"}
	pre := repeat(vbucket(100, 0, 0), minPreBuckets-1) // one short
	post := repeat(vbucket(200, 0, 0), 6)

	if _, ok := ScoreOutcome(sig, pre, post, 6); ok {
		t.Errorf("expected ok=false with %d pre buckets (min %d)", len(pre), minPreBuckets)
	}
}

// An empty post window is not gradeable: ok=false.
func TestScoreOutcome_EmptyPostWindow(t *testing.T) {
	sig := store.Signal{ID: 7, Pool: "0xpool", SignalType: 1, Metric: "vol0"}
	pre := repeat(vbucket(100, 0, 0), 6)

	if _, ok := ScoreOutcome(sig, pre, nil, 6); ok {
		t.Errorf("expected ok=false with an empty post window")
	}
}

// A non-positive pre_median (baseline all zero on the measured metric) is not
// gradeable: the ratios would be undefined, so ok=false.
func TestScoreOutcome_ZeroPreMedian(t *testing.T) {
	sig := store.Signal{ID: 8, Pool: "0xpool", SignalType: 1, Metric: "vol0"}
	pre := repeat(vbucket(0, 0, 0), 6) // pre_median = 0
	post := repeat(vbucket(200, 0, 0), 6)

	if _, ok := ScoreOutcome(sig, pre, post, 6); ok {
		t.Errorf("expected ok=false when pre_median <= 0")
	}
}

// A whale signal (metric whale_swap) is graded on vol0 follow-through and gets no
// direction facet (whale is not a netflow metric).
func TestScoreOutcome_WhaleUsesVol0NoDirection(t *testing.T) {
	sig := store.Signal{
		ID: 9, Pool: "0xpool", SignalType: 2, Metric: "whale_swap",
		EventRef: "0xabc:3",
	}
	pre := repeat(vbucket(100, 0, 0), 6)
	post := repeat(vbucket(150, 0, 0), 6) // 1.5x token0 volume after the whale

	card, ok := ScoreOutcome(sig, pre, post, 6)
	if !ok {
		t.Fatalf("expected ok=true for a gradeable whale signal")
	}
	assertGradeInvariants(t, card)
	if card.Facets.FollowThrough != 1.5 {
		t.Errorf("whale follow_through = %v, want 1.5", card.Facets.FollowThrough)
	}
	if card.Facets.Direction != nil || card.Subs.DC != nil {
		t.Errorf("whale must have nil direction/DC, got dir=%v dc=%v", card.Facets.Direction, card.Subs.DC)
	}
}

// ReportHash must be deterministic and reproducible from a card, and the
// canonical card bytes must re-hash to the same value (the stored-card contract).
func TestReportHash_ReproducibleFromCard(t *testing.T) {
	sig := store.Signal{ID: 10, Pool: "0xPooL", SignalType: 1, Metric: "vol0"}
	pre := repeat(vbucket(100, 0, 0), 6)
	post := repeat(vbucket(220, 0, 0), 6)

	card, ok := ScoreOutcome(sig, pre, post, 6)
	if !ok {
		t.Fatalf("expected ok=true")
	}

	h1, err := ReportHash(card)
	if err != nil {
		t.Fatalf("ReportHash: %v", err)
	}
	h2, err := ReportHash(card)
	if err != nil {
		t.Fatalf("ReportHash (2nd): %v", err)
	}
	if h1 != h2 {
		t.Errorf("ReportHash not deterministic: %s != %s", h1, h2)
	}
	if len(h1) != 66 || h1[:2] != "0x" { // 0x + 32 bytes hex
		t.Errorf("ReportHash malformed: %q", h1)
	}

	// Re-marshaling the stored card must reproduce the same canonical bytes that
	// were hashed: the property that lets a stored card be re-verified.
	b1, err := canonicalCardJSON(card)
	if err != nil {
		t.Fatalf("canonicalCardJSON: %v", err)
	}
	var round Card
	if err := json.Unmarshal(b1, &round); err != nil {
		t.Fatalf("unmarshal canonical card: %v", err)
	}
	b2, err := canonicalCardJSON(round)
	if err != nil {
		t.Fatalf("canonicalCardJSON (round): %v", err)
	}
	if string(b1) != string(b2) {
		t.Errorf("canonical card not stable across a marshal round-trip:\n %s\n %s", b1, b2)
	}
	// Pool is lowercased in the card regardless of the signal's casing.
	if card.Pool != "0xpool" {
		t.Errorf("card pool not lowercased: %q", card.Pool)
	}
}

// The canonical encoding must also be stable when the optional pointer facets
// (direction/DC) are present: null vs a number both have to round-trip
// byte-identically, since the hash covers them.
func TestReportHash_ReproducibleWithDirection(t *testing.T) {
	payload, _ := json.Marshal(map[string]float64{"latest": 1000})
	sig := store.Signal{ID: 11, Pool: "0xpool", SignalType: 1, Metric: "netflow0", Payload: payload}
	pre := repeat(vbucket(100, 0, 0), 6)
	post := repeat(vbucket(200, 0, 500), 6)

	card, ok := ScoreOutcome(sig, pre, post, 6)
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if card.Subs.DC == nil {
		t.Fatalf("precondition: expected a present DC sub-score")
	}

	b1, err := canonicalCardJSON(card)
	if err != nil {
		t.Fatalf("canonicalCardJSON: %v", err)
	}
	var round Card
	if err := json.Unmarshal(b1, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	b2, err := canonicalCardJSON(round)
	if err != nil {
		t.Fatalf("canonicalCardJSON (round): %v", err)
	}
	if string(b1) != string(b2) {
		t.Errorf("canonical card with direction not stable:\n %s\n %s", b1, b2)
	}
}
