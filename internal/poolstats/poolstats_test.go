package poolstats

import (
	"context"
	"errors"
	"testing"

	"ocr/internal/marketdata"
	"ocr/internal/store"
)

// Hermetic tests for the poolstats stage: a mock market source + mock store exercise the
// refresh path, the snapshot-failure and per-pool-failure best-effort guarantees, the
// lowercase address lookup, and the nil-field -> NULL conversion. No chain, no DB.

type mockMarket struct {
	snap map[string]marketdata.PoolMarketStats
	err  error
	got  []store.Pool // pools the stage passed to Snapshot
}

func (m *mockMarket) Snapshot(_ context.Context, pools []store.Pool) (map[string]marketdata.PoolMarketStats, error) {
	m.got = pools
	if m.err != nil {
		return nil, m.err
	}
	return m.snap, nil
}

type mockStore struct {
	pools     []store.Pool
	listErr   error
	upserts   []store.PoolStats
	upsertErr map[string]error
}

func (m *mockStore) ListEnabledPools(_ context.Context) ([]store.Pool, error) {
	return m.pools, m.listErr
}

func (m *mockStore) UpsertPoolStats(_ context.Context, ps store.PoolStats) error {
	if err, ok := m.upsertErr[ps.Pool]; ok {
		return err
	}
	m.upserts = append(m.upserts, ps)
	return nil
}

func f64(v float64) *float64 { return &v }

// TestRunOnceUpsertsSnapshot: the stage hands the enabled pools to the source and upserts
// the returned snapshot, carrying the numeric fields and the provenance source.
func TestRunOnceUpsertsSnapshot(t *testing.T) {
	st := &mockStore{pools: []store.Pool{{Address: "0xa", Enabled: true}}}
	mk := &mockMarket{snap: map[string]marketdata.PoolMarketStats{
		"0xa": {PoolAddr: "0xa", TVLUSD: f64(1234.5), Vol24hUSD: f64(67.8), PriceUSD: f64(1.001), SourceURL: "on-chain"},
	}}
	s := New(st, mk, 0) // interval irrelevant for RunOnce

	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(mk.got) != 1 || mk.got[0].Address != "0xa" {
		t.Fatalf("Snapshot should receive the enabled pools, got %v", mk.got)
	}
	if len(st.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1", len(st.upserts))
	}
	got := st.upserts[0]
	if got.Pool != "0xa" || got.TVLUSD == nil || got.TVLUSD.String() != "1234.5" {
		t.Fatalf("upsert tvl wrong: %+v", got)
	}
	if got.PriceUSD == nil || got.PriceUSD.String() != "1.001" {
		t.Fatalf("upsert price wrong: %+v", got)
	}
	if got.SourceURL != "on-chain" {
		t.Fatalf("upsert source wrong: %+v", got)
	}
	if got.FetchedAt.IsZero() {
		t.Fatal("fetched_at should be stamped")
	}
}

// TestRunOnceNilFieldsBecomeNull: a snapshot with missing numeric fields (nil) produces
// nil decimals on the store row, so the columns stay NULL, not 0.
func TestRunOnceNilFieldsBecomeNull(t *testing.T) {
	st := &mockStore{pools: []store.Pool{{Address: "0xa", Enabled: true}}}
	mk := &mockMarket{snap: map[string]marketdata.PoolMarketStats{"0xa": {PoolAddr: "0xa", SourceURL: "on-chain"}}}
	s := New(st, mk, 0)
	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got := st.upserts[0]
	if got.TVLUSD != nil || got.Vol24hUSD != nil || got.PriceUSD != nil || got.PriceChange24h != nil {
		t.Fatalf("nil snapshot fields must map to nil decimals, got %+v", got)
	}
}

// TestRunOnceSnapshotErrorNoUpserts: a whole-snapshot failure is best-effort — logged,
// nothing upserted, and RunOnce still returns nil (the caller retries next tick).
func TestRunOnceSnapshotErrorNoUpserts(t *testing.T) {
	st := &mockStore{pools: []store.Pool{{Address: "0xa", Enabled: true}}}
	mk := &mockMarket{err: errors.New("rpc down")}
	s := New(st, mk, 0)
	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("best-effort RunOnce should swallow the snapshot error, got %v", err)
	}
	if len(st.upserts) != 0 {
		t.Fatalf("a snapshot failure must upsert nothing, got %d", len(st.upserts))
	}
}

// TestRunOncePerPoolUpsertFailureContinues: one pool's upsert error must not abort the
// pass; the other pool still lands.
func TestRunOncePerPoolUpsertFailureContinues(t *testing.T) {
	st := &mockStore{
		pools:     []store.Pool{{Address: "0xbad", Enabled: true}, {Address: "0xgood", Enabled: true}},
		upsertErr: map[string]error{"0xbad": errors.New("db blip")},
	}
	mk := &mockMarket{snap: map[string]marketdata.PoolMarketStats{
		"0xbad":  {PoolAddr: "0xbad", TVLUSD: f64(1), SourceURL: "on-chain"},
		"0xgood": {PoolAddr: "0xgood", TVLUSD: f64(2), SourceURL: "on-chain"},
	}}
	s := New(st, mk, 0)
	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(st.upserts) != 1 || st.upserts[0].Pool != "0xgood" {
		t.Fatalf("only the good pool should upsert, got %+v", st.upserts)
	}
}

// TestRunOncePoolAbsentFromSnapshotSkipped: a pool the source did not return is skipped
// (defensive), not upserted with empty values.
func TestRunOncePoolAbsentFromSnapshotSkipped(t *testing.T) {
	st := &mockStore{pools: []store.Pool{{Address: "0xa", Enabled: true}, {Address: "0xb", Enabled: true}}}
	mk := &mockMarket{snap: map[string]marketdata.PoolMarketStats{"0xa": {PoolAddr: "0xa", TVLUSD: f64(1), SourceURL: "on-chain"}}}
	s := New(st, mk, 0)
	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(st.upserts) != 1 || st.upserts[0].Pool != "0xa" {
		t.Fatalf("a pool absent from the snapshot must be skipped, got %+v", st.upserts)
	}
}

// TestRunOnceMixedCaseAddressLookup: the stage lowercases the registry address before the
// snapshot lookup, so a mixed-case registry row still resolves.
func TestRunOnceMixedCaseAddressLookup(t *testing.T) {
	st := &mockStore{pools: []store.Pool{{Address: "0xAbC", Enabled: true}}}
	mk := &mockMarket{snap: map[string]marketdata.PoolMarketStats{"0xabc": {PoolAddr: "0xabc", TVLUSD: f64(7), SourceURL: "on-chain"}}}
	s := New(st, mk, 0)
	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(st.upserts) != 1 {
		t.Fatalf("mixed-case address should resolve via lowercase, got %+v", st.upserts)
	}
}

// TestRunDisabledIsNoOp: Run with a non-positive interval returns nil immediately (the
// supervisor gate), touching neither source nor store.
func TestRunDisabledIsNoOp(t *testing.T) {
	st := &mockStore{pools: []store.Pool{{Address: "0xa", Enabled: true}}}
	mk := &mockMarket{}
	s := New(st, mk, 0)
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("disabled Run should return nil, got %v", err)
	}
	if mk.got != nil || len(st.upserts) != 0 {
		t.Fatal("disabled stage must not snapshot or upsert")
	}
}

// TestToStorePoolStatsConversion pins the float->decimal (nil-preserving) mapping.
func TestToStorePoolStatsConversion(t *testing.T) {
	ms := marketdata.PoolMarketStats{PoolAddr: "0xa", TVLUSD: f64(100.25), PriceChange24h: f64(-1.5), SourceURL: "on-chain"}
	got := toStorePoolStats(ms)
	if got.TVLUSD == nil || got.TVLUSD.String() != "100.25" {
		t.Fatalf("tvl = %v", got.TVLUSD)
	}
	if got.PriceChange24h == nil || got.PriceChange24h.String() != "-1.5" {
		t.Fatalf("change = %v", got.PriceChange24h)
	}
	if got.Vol24hUSD != nil || got.PriceUSD != nil {
		t.Fatalf("absent fields should be nil: %+v", got)
	}
}
