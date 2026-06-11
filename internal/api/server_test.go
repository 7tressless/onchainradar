package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"ocr/internal/store"
)

// These are handler integration tests gated on OCR_TEST_DATABASE_URL: they spin a
// schema-isolated Postgres, apply the real migrations, seed deterministic
// fixtures, then drive the handlers through httptest. When the env var is unset
// they skip, so `go test ./...` stays green without a DB.
//
// They cover the JSON shape end-to-end (stats, signals list + filters + pagination,
// detail, 404/400, pools/actors/series) plus an SSE smoke test (replay + a live
// broadcast through the Hub).

const testDBEnv = "OCR_TEST_DATABASE_URL"

// newTestServer builds a schema-isolated store, applies migrations, seeds fixtures,
// and returns an *Server (not listening) plus the underlying DB. It registers
// cleanup that drops the schema. Skips when OCR_TEST_DATABASE_URL is unset.
func newTestServer(t *testing.T) (*Server, *store.DB) {
	t.Helper()
	dsn := os.Getenv(testDBEnv)
	if dsn == "" {
		t.Skipf("%s not set; skipping API handler integration test", testDBEnv)
	}

	schema := fmt.Sprintf("ocr_api_test_%d_%d", time.Now().UnixNano(), rand.Intn(1_000_000)) //nolint:gosec // test-only schema name

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema

	ctx := context.Background()

	// Create the schema on a bootstrap connection (no search_path pin).
	bootCfg := cfg.ConnConfig.Copy()
	delete(bootCfg.RuntimeParams, "search_path")
	boot, err := pgx.Connect(ctx, bootCfg.ConnString())
	if err != nil {
		t.Fatalf("bootstrap connect: %v", err)
	}
	if _, err := boot.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		_ = boot.Close(ctx)
		t.Fatalf("create schema: %v", err)
	}
	_ = boot.Close(ctx)

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	db := &store.DB{Pool: pool}

	t.Cleanup(func() {
		dropCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		bc := bootCfg.Copy()
		c, derr := pgx.Connect(dropCtx, bc.ConnString())
		if derr == nil {
			_, _ = c.Exec(dropCtx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			_ = c.Close(dropCtx)
		}
		db.Close()
	})

	applyMigrations(t, ctx, pool, schema)
	seedFixtures(t, ctx, db)

	srv := NewServer(db, Config{Addr: "127.0.0.1:0", ExplorerBase: testExplorer})
	srv.refreshLabels(ctx)
	return srv, db
}

// applyMigrations runs every migrations/*.sql (lexical order) inside the test
// schema. The migration files are repo-root-relative; this test file is two
// directories deep (internal/api), so resolve via ../../migrations.
func applyMigrations(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob migrations: %v (found %d)", err, len(files))
	}
	sort.Strings(files)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire for migrations: %v", err)
	}
	defer conn.Release()
	// search_path is already pinned to the test schema on pooled conns, so the
	// migrations create their tables inside it.
	for _, f := range files {
		sqlBytes, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatalf("read migration %s: %v", f, rerr)
		}
		if _, eerr := conn.Exec(ctx, string(sqlBytes)); eerr != nil {
			t.Fatalf("apply migration %s: %v", f, eerr)
		}
	}
}

// seedFixtures inserts a small, deterministic dataset: 2 pools, 3 signals (one
// flow + attest + outcome, one whale, one smart-money with actor), and a pool_stats
// row, so every handler has something to return and filters are exercised.
func seedFixtures(t *testing.T, ctx context.Context, db *store.DB) {
	t.Helper()
	const poolA = "0xaaaa000000000000000000000000000000000001"
	const poolB = "0xbbbb000000000000000000000000000000000002"
	const actor = "0xacc0000000000000000000000000000000000003"

	for _, p := range []store.Pool{
		{Address: poolA, Dex: "agni", Token0: "0xt0", Token1: "0xt1", Dec0: 18, Dec1: 18, Label: "USDe/WMNT", Enabled: true, SourceURL: "src/a"},
		{Address: poolB, Dex: "merchant_moe", Token0: "0xt2", Token1: "0xt3", Dec0: 18, Dec1: 6, Label: "USDC/USDe", Enabled: true, SourceURL: "src/b"},
	} {
		if err := db.UpsertPool(ctx, p); err != nil {
			t.Fatalf("seed pool: %v", err)
		}
	}

	now := time.Now().UTC()
	// Flow signal (type 1) with attest tx + a bucket; will also get an outcome.
	flow := store.Signal{
		Pool: poolA, SignalType: 1, Metric: "vol1", Zscore: 5.14, WindowBuckets: 144,
		BucketTS: now.Add(-2 * time.Hour), Payload: json.RawMessage(`{"latest":7.8,"median":1.0}`),
		LLMNote: "flow note", AttestTx: "0xflowtx", Status: "submitted",
	}
	flowID, ins, err := db.InsertSignal(ctx, flow)
	if err != nil || !ins {
		t.Fatalf("seed flow signal: ins=%v err=%v", ins, err)
	}
	// Whale signal (type 2), event-keyed.
	whale := store.Signal{
		Pool: poolB, SignalType: 2, Metric: "whale_swap", Zscore: 30, WindowBuckets: 200,
		BucketTS: now.Add(-90 * time.Minute), EventRef: "0xwtx:1",
		Payload: json.RawMessage(`{"size0":1000,"multiple":40}`), Status: "alerted",
	}
	if _, ins, err := db.InsertSignal(ctx, whale); err != nil || !ins {
		t.Fatalf("seed whale signal: ins=%v err=%v", ins, err)
	}
	// Smart-money signal (type 3) with an actor.
	sm := store.Signal{
		Pool: poolA, SignalType: 3, Metric: "smart_money", Zscore: 77, WindowBuckets: 4,
		BucketTS: now.Add(-30 * time.Minute), EventRef: "0xstx:0", Actor: actor,
		Payload: json.RawMessage(`{"actor":"` + actor + `","accum_swaps":4,"distinct_pools":2,"score":77}`),
		Status:  "new",
	}
	smID, ins, err := db.InsertSignal(ctx, sm)
	if err != nil || !ins {
		t.Fatalf("seed smart-money signal: ins=%v err=%v", ins, err)
	}
	_ = smID

	// An outcome on the flow signal (graded B, attested).
	card := json.RawMessage(`{"signal_id":` + fmt.Sprint(flowID) + `,"grade":82}`)
	if _, err := db.InsertOutcome(ctx, store.Outcome{
		SignalID: flowID, HorizonHours: 6, Grade: 82, Letter: "B", Status: "confirmed",
		ReportHash: "0xreporthash", Card: card,
	}); err != nil {
		t.Fatalf("seed outcome: %v", err)
	}
	if err := db.UpdateOutcomeAttest(ctx, flowID, "0xoutcometx"); err != nil {
		t.Fatalf("seed outcome attest: %v", err)
	}

	// A large_swaps ledger row for the actor (24h activity) so the leaderboard has
	// non-zero swaps.
	if _, err := db.InsertLargeSwap(ctx, store.LargeSwap{
		TxHash: "0xstx", LogIndex: 0, Pool: poolA, Actor: actor,
		SizeToken0: mustDec("1000"), Net0: mustDec("1000"), Net1: mustDec("-1000"),
		BlockTime: now.Add(-20 * time.Minute),
	}); err != nil {
		t.Fatalf("seed large_swap: %v", err)
	}

	// A pool_stats row for poolA (external display-only stats).
	tvl := mustDec("3300225.06")
	vol := mustDec("234704.09")
	price := mustDec("1.0012")
	chg := mustDec("0.111")
	if err := db.UpsertPoolStats(ctx, store.PoolStats{
		Pool: poolA, TVLUSD: &tvl, Vol24hUSD: &vol, PriceUSD: &price, PriceChange24h: &chg,
		BaseToken: "USDe", QuoteToken: "WMNT", SourceURL: "https://gecko/a",
	}); err != nil {
		t.Fatalf("seed pool_stats: %v", err)
	}
}

// do issues a GET against the server's full middleware-wrapped handler and returns
// the recorder.
func do(srv *Server, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, nil)
	srv.routes().ServeHTTP(rec, req)
	return rec
}

func TestHandleStats(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := do(srv, http.MethodGet, "/api/stats")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var dto StatsDTO
	mustJSON(t, rec, &dto)

	if dto.TotalSignals != 3 {
		t.Fatalf("total_signals = %d, want 3", dto.TotalSignals)
	}
	if dto.ByType.Flow != 1 || dto.ByType.Whale != 1 || dto.ByType.SmartMoney != 1 {
		t.Fatalf("by_type = %+v, want 1/1/1", dto.ByType)
	}
	if dto.SignalAttestations != 1 {
		t.Fatalf("signal_attestations = %d, want 1", dto.SignalAttestations)
	}
	if dto.OutcomesTotal != 1 || dto.GradeDistribution.B != 1 {
		t.Fatalf("outcomes wrong: total=%d B=%d", dto.OutcomesTotal, dto.GradeDistribution.B)
	}
	if dto.OutcomeAttestations != 1 {
		t.Fatalf("outcome_attestations = %d, want 1", dto.OutcomeAttestations)
	}
	if dto.PoolsTracked != 2 {
		t.Fatalf("pools_tracked = %d, want 2", dto.PoolsTracked)
	}
	if dto.Last24h.SmartMoney != 1 {
		t.Fatalf("last_24h.smart_money = %d, want 1", dto.Last24h.SmartMoney)
	}
	if dto.Last24h.TopActor == nil {
		t.Fatal("last_24h.top_actor should be set (the smart-money actor)")
	}
	if dto.FirstSignalAt == nil {
		t.Fatal("first_signal_at should be set")
	}
}

func TestHandleSignalsListAndFilters(t *testing.T) {
	srv, _ := newTestServer(t)

	// No filter: all 3, newest first.
	rec := do(srv, http.MethodGet, "/api/signals")
	var list SignalListDTO
	mustJSON(t, rec, &list)
	if list.Total != 3 || len(list.Items) != 3 {
		t.Fatalf("unfiltered total=%d items=%d, want 3/3", list.Total, len(list.Items))
	}
	if list.Limit != defaultSignalLimit || list.Offset != 0 {
		t.Fatalf("pagination meta = limit %d offset %d", list.Limit, list.Offset)
	}

	// Filter by type=3 (smart-money): exactly 1, with a non-null actor.
	rec = do(srv, http.MethodGet, "/api/signals?type=3")
	mustJSON(t, rec, &list)
	if list.Total != 1 || len(list.Items) != 1 {
		t.Fatalf("type=3 total=%d items=%d, want 1/1", list.Total, len(list.Items))
	}
	if list.Items[0].TypeName != "smart_money" || list.Items[0].Actor == nil {
		t.Fatalf("type=3 item wrong: %+v", list.Items[0])
	}

	// Filter by actor.
	actor := *list.Items[0].Actor
	rec = do(srv, http.MethodGet, "/api/signals?actor="+actor)
	mustJSON(t, rec, &list)
	if list.Total != 1 {
		t.Fatalf("actor filter total = %d, want 1", list.Total)
	}

	// Pagination: limit=2 offset=0 then offset=2.
	rec = do(srv, http.MethodGet, "/api/signals?limit=2&offset=0")
	mustJSON(t, rec, &list)
	if len(list.Items) != 2 || list.Total != 3 {
		t.Fatalf("page1 items=%d total=%d, want 2/3", len(list.Items), list.Total)
	}
	firstPageTop := list.Items[0].ID
	rec = do(srv, http.MethodGet, "/api/signals?limit=2&offset=2")
	mustJSON(t, rec, &list)
	if len(list.Items) != 1 {
		t.Fatalf("page2 items=%d, want 1", len(list.Items))
	}
	if list.Items[0].ID == firstPageTop {
		t.Fatal("page2 overlaps page1 (offset not applied)")
	}

	// Bad type -> 400.
	rec = do(srv, http.MethodGet, "/api/signals?type=abc")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad type status = %d, want 400", rec.Code)
	}
}

func TestHandleSignalByID(t *testing.T) {
	srv, _ := newTestServer(t)
	// Grab a real id from the list.
	rec := do(srv, http.MethodGet, "/api/signals?type=1")
	var list SignalListDTO
	mustJSON(t, rec, &list)
	id := list.Items[0].ID

	rec = do(srv, http.MethodGet, fmt.Sprintf("/api/signals/%d", id))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var detail SignalDetailDTO
	mustJSON(t, rec, &detail)
	if detail.ID != id || detail.WindowBuckets != 144 {
		t.Fatalf("detail wrong: %+v", detail)
	}
	// Payload must be a real object with the seeded keys.
	var pl map[string]any
	if err := json.Unmarshal(detail.Payload, &pl); err != nil {
		t.Fatalf("payload not an object: %v", err)
	}
	if pl["latest"] != 7.8 {
		t.Fatalf("payload latest = %v, want 7.8", pl["latest"])
	}
	if detail.Outcome == nil || detail.Outcome.Letter != "B" {
		t.Fatalf("detail outcome wrong: %+v", detail.Outcome)
	}

	// 404 for missing id, 400 for junk.
	if rec := do(srv, http.MethodGet, "/api/signals/99999999"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing id status = %d, want 404", rec.Code)
	}
	if rec := do(srv, http.MethodGet, "/api/signals/abc"); rec.Code != http.StatusBadRequest {
		t.Fatalf("junk id status = %d, want 400", rec.Code)
	}
}

func TestHandlePools(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := do(srv, http.MethodGet, "/api/pools")
	var list PoolListDTO
	mustJSON(t, rec, &list)
	if len(list.Items) != 2 {
		t.Fatalf("pools = %d, want 2", len(list.Items))
	}
	// poolA has stats; poolB does not.
	var withStats, withoutStats int
	for _, p := range list.Items {
		if p.Stats != nil {
			withStats++
			if p.Stats.TVLUSD == nil || *p.Stats.TVLUSD != "3300225.06" {
				t.Fatalf("poolA tvl wrong: %+v", p.Stats)
			}
		} else {
			withoutStats++
		}
		// Activity block always present.
		if p.Activity.Vol0_24h == "" {
			t.Fatalf("activity vol0 should be a string (zero ok), pool %s", p.Address)
		}
	}
	if withStats != 1 || withoutStats != 1 {
		t.Fatalf("expected 1 pool with stats and 1 without, got %d/%d", withStats, withoutStats)
	}
}

func TestHandlePoolByAddrDetail(t *testing.T) {
	srv, _ := newTestServer(t)
	const poolA = "0xaaaa000000000000000000000000000000000001"
	rec := do(srv, http.MethodGet, "/api/pools/"+poolA)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var detail PoolDetailDTO
	mustJSON(t, rec, &detail)
	if detail.Address != poolA || detail.Label != "USDe/WMNT" {
		t.Fatalf("detail header wrong: %+v", detail.PoolDTO)
	}
	// recent_signals must include the flow + smart-money signals on poolA (2).
	if len(detail.RecentSignals) != 2 {
		t.Fatalf("recent_signals = %d, want 2", len(detail.RecentSignals))
	}
	// series is non-nil (empty is fine; no buckets seeded).
	if detail.Series == nil {
		t.Fatal("series should be non-nil")
	}

	if rec := do(srv, http.MethodGet, "/api/pools/0xdeadbeef"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing pool status = %d, want 404", rec.Code)
	}
}

func TestHandleActors(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := do(srv, http.MethodGet, "/api/actors")
	var list ActorListDTO
	mustJSON(t, rec, &list)
	if len(list.Items) != 1 {
		t.Fatalf("actors = %d, want 1", len(list.Items))
	}
	a := list.Items[0]
	if a.TotalSignals != 1 || a.LargeSwaps24h != 1 || a.PoolsTouched24h != 1 {
		t.Fatalf("actor counts wrong: %+v", a)
	}
	if a.ActorURL == "" {
		t.Fatal("actor_url should be set")
	}

	// Detail.
	rec = do(srv, http.MethodGet, "/api/actors/"+a.Actor)
	if rec.Code != http.StatusOK {
		t.Fatalf("actor detail status = %d", rec.Code)
	}
	var detail ActorDetailDTO
	mustJSON(t, rec, &detail)
	if len(detail.Signals) != 1 {
		t.Fatalf("actor detail signals = %d, want 1", len(detail.Signals))
	}
	if detail.Accumulation.WindowHours != defaultActorWindowHours {
		t.Fatalf("accumulation window = %d", detail.Accumulation.WindowHours)
	}
	if detail.Accumulation.Swaps != 1 {
		t.Fatalf("accumulation swaps = %d, want 1", detail.Accumulation.Swaps)
	}

	if rec := do(srv, http.MethodGet, "/api/actors/0xnope"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing actor status = %d, want 404", rec.Code)
	}
}

func TestHandleSeries(t *testing.T) {
	srv, _ := newTestServer(t)
	const poolA = "0xaaaa000000000000000000000000000000000001"
	rec := do(srv, http.MethodGet, "/api/series?pool="+poolA+"&metric=swap_count&hours=24")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var s SeriesDTO
	mustJSON(t, rec, &s)
	if s.Pool != poolA || s.Metric != "swap_count" {
		t.Fatalf("series header wrong: %+v", s)
	}
	if s.Points == nil {
		t.Fatal("points should be non-nil (empty ok)")
	}

	// Missing pool / bad metric -> 400.
	if rec := do(srv, http.MethodGet, "/api/series?metric=swap_count"); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing pool status = %d, want 400", rec.Code)
	}
	if rec := do(srv, http.MethodGet, "/api/series?pool="+poolA+"&metric=evil"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad metric status = %d, want 400", rec.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := do(srv, http.MethodPost, "/api/stats")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); !strings.Contains(got, "GET") {
		t.Fatalf("Allow = %q", got)
	}
}

// TestSSESmoke connects to the SSE handler through an httptest.Server, asserts the
// connect-time replay delivers the seeded signals, then inserts a new signal and
// asserts it arrives as a live event via the Hub tail.
func TestSSESmoke(t *testing.T) {
	srv, db := newTestServer(t)

	// Run the server on a real listener so SSE streaming (flushing) works; httptest
	// recorders do not stream.
	ctx, cancel := context.WithCancel(context.Background())
	ts := httptest.NewServer(srv.routes())
	t.Cleanup(func() { cancel(); ts.Close(); srv.hub.Close() })

	// Start the live tail (the producer) against the same DB+hub.
	go func() { _ = srv.runLiveTail(ctx) }()

	// Connect; default (no Last-Event-ID) replays the recent window (our 3 signals).
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/live", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sse connect: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	// Read replayed events: expect at least the 3 seeded signal ids.
	ids := readSSEIDs(t, resp, 3, 5*time.Second)
	if len(ids) < 3 {
		t.Fatalf("replay delivered %d ids, want >= 3", len(ids))
	}
	maxSeen := ids[len(ids)-1]

	// Insert a new signal; the tail (2s poll) should broadcast it live.
	newSig := store.Signal{
		Pool: "0xaaaa000000000000000000000000000000000001", SignalType: 3,
		Metric: "smart_money", Zscore: 90, WindowBuckets: 5,
		BucketTS: time.Now().UTC(), EventRef: "0xlive:0",
		Actor:   "0xacc0000000000000000000000000000000000003",
		Payload: json.RawMessage(`{"score":90}`), Status: "new",
	}
	newID, ins, err := db.InsertSignal(context.Background(), newSig)
	if err != nil || !ins {
		t.Fatalf("insert live signal: ins=%v err=%v", ins, err)
	}
	if newID <= maxSeen {
		t.Fatalf("new id %d not greater than replayed max %d", newID, maxSeen)
	}

	// Expect the new id to arrive within a few tail intervals.
	live := readSSEIDs(t, resp, 1, 8*time.Second)
	found := false
	for _, id := range live {
		if id == newID {
			found = true
		}
	}
	if !found {
		t.Fatalf("live signal id %d not received over SSE (got %v)", newID, live)
	}
}

// readSSEIDs reads from an SSE response body until it has collected `want` "id:"
// lines or the deadline elapses, returning the ids in order.
func readSSEIDs(t *testing.T, resp *http.Response, want int, timeout time.Duration) []int64 {
	t.Helper()
	type res struct {
		ids []int64
	}
	done := make(chan res, 1)
	go func() {
		var ids []int64
		buf := make([]byte, 4096)
		var acc strings.Builder
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				acc.WriteString(string(buf[:n]))
				for _, line := range strings.Split(acc.String(), "\n") {
					if strings.HasPrefix(line, "id: ") {
						var id int64
						_, _ = fmt.Sscanf(strings.TrimSpace(line), "id: %d", &id)
						if !containsID(ids, id) {
							ids = append(ids, id)
						}
					}
				}
				if len(ids) >= want {
					done <- res{ids}
					return
				}
			}
			if err != nil {
				done <- res{ids}
				return
			}
		}
	}()
	select {
	case r := <-done:
		return r.ids
	case <-time.After(timeout):
		return nil
	}
}

func containsID(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func mustJSON(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("unmarshal %T: %v\nbody: %s", v, err, rec.Body.String())
	}
}

func mustDec(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}
