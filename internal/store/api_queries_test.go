package store

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB-gated integration tests for the API store queries (token_meta upsert
// + join, raw_logs point-lookup by event reference, and the actor recent-large-swaps
// read). They skip when OCR_TEST_DATABASE_URL is unset, and each runs inside its own
// randomly-named throwaway schema that is dropped on cleanup, never touching the
// public schema or any live table (same isolation contract as queries_signal_test.go).

// newClarityTestDB opens a schema-isolated *DB and creates just the tables these
// queries touch: token_meta (0010), raw_logs (0001), pools (0001), and large_swaps
// (0006 + net0/net1 from 0008). Kept minimal + in lock-step with the columns the
// queries under test SELECT/INSERT.
func newClarityTestDB(t *testing.T) *DB {
	t.Helper()

	dsn := os.Getenv(testDBEnv)
	if dsn == "" {
		t.Skipf("%s not set; skipping store integration test", testDBEnv)
	}

	schema := fmt.Sprintf("ocr_test_%d_%d", time.Now().UnixNano(), rand.Intn(1_000_000)) //nolint:gosec // test-only schema name

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema

	ctx := context.Background()

	bootCfg := cfg.ConnConfig.Copy()
	delete(bootCfg.RuntimeParams, "search_path")
	bootConn, err := pgx.Connect(ctx, bootCfg.ConnString())
	if err != nil {
		t.Fatalf("bootstrap connect: %v", err)
	}
	if _, err := bootConn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		bootConn.Close(ctx)
		t.Fatalf("create schema %s: %v", schema, err)
	}
	bootConn.Close(ctx)

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}

	const ddl = `
		CREATE TABLE token_meta (
			address    TEXT PRIMARY KEY,
			symbol     TEXT,
			logo_url   TEXT,
			decimals   INT,
			fetched_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			source_url TEXT
		);
		CREATE TABLE pools (
			address    TEXT PRIMARY KEY,
			dex        TEXT NOT NULL,
			token0     TEXT NOT NULL,
			token1     TEXT NOT NULL,
			dec0       INT  NOT NULL,
			dec1       INT  NOT NULL,
			label      TEXT,
			enabled    BOOLEAN NOT NULL DEFAULT TRUE,
			source_url TEXT,
			z_threshold        DOUBLE PRECISION,
			whale_min_multiple DOUBLE PRECISION
		);
		CREATE TABLE raw_logs (
			chain_id     BIGINT  NOT NULL,
			block_number BIGINT  NOT NULL,
			block_time   TIMESTAMPTZ NOT NULL,
			tx_hash      TEXT    NOT NULL,
			log_index    INTEGER NOT NULL,
			address      TEXT    NOT NULL,
			topic0       TEXT,
			topic1       TEXT,
			topic2       TEXT,
			topic3       TEXT,
			data         TEXT,
			PRIMARY KEY (chain_id, tx_hash, log_index)
		);
		CREATE TABLE large_swaps (
			tx_hash     TEXT        NOT NULL,
			log_index   INTEGER     NOT NULL,
			pool        TEXT        NOT NULL,
			actor       TEXT        NOT NULL,
			size_token0 NUMERIC(78) NOT NULL,
			net0        NUMERIC(78),
			net1        NUMERIC(78),
			block_time  TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (tx_hash, log_index)
		);`
	if _, err := pool.Exec(ctx, ddl); err != nil {
		pool.Close()
		dropSchema(t, bootCfg.ConnString(), schema)
		t.Fatalf("create tables: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		dropSchema(t, bootCfg.ConnString(), schema)
	})
	return &DB{Pool: pool}
}

func intp(v int) *int { return &v }

// TestUpsertTokenMeta_AndJoin verifies the token_meta upsert (insert + idempotent
// update by address) and the multi-address join used by the API: only present
// addresses appear, decimals round-trip (including NULL), and empty fields stay "".
func TestUpsertTokenMeta_AndJoin(t *testing.T) {
	db := newClarityTestDB(t)
	ctx := context.Background()

	// Insert two tokens; one with full metadata, one with a NULL decimals + no logo.
	if err := db.UpsertTokenMeta(ctx, TokenMeta{
		Address: "0xTok0", Symbol: "USDe", LogoURL: "https://logo/usde.png",
		Decimals: intp(18), SourceURL: "u/a",
	}); err != nil {
		t.Fatalf("upsert tok0: %v", err)
	}
	if err := db.UpsertTokenMeta(ctx, TokenMeta{
		Address: "0xTok1", Symbol: "WMNT", LogoURL: "", Decimals: nil, SourceURL: "u/a",
	}); err != nil {
		t.Fatalf("upsert tok1: %v", err)
	}

	// Join by a set that includes an UNKNOWN address (must simply be absent) and
	// mixes case (must match the lowercase-stored key).
	got, err := db.TokenMetaByAddresses(ctx, []string{"0xTOK0", "0xtok1", "0xunknown"})
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("join returned %d rows, want 2: %+v", len(got), got)
	}
	t0, ok := got["0xtok0"]
	if !ok || t0.Symbol != "USDe" || t0.LogoURL != "https://logo/usde.png" || t0.Decimals == nil || *t0.Decimals != 18 {
		t.Fatalf("tok0 join wrong: %+v (ok=%v)", t0, ok)
	}
	t1, ok := got["0xtok1"]
	if !ok || t1.Symbol != "WMNT" || t1.LogoURL != "" || t1.Decimals != nil {
		t.Fatalf("tok1 join wrong: %+v (ok=%v)", t1, ok)
	}

	// Idempotent UPDATE: re-upsert tok0 with new values; the row must be replaced.
	if err := db.UpsertTokenMeta(ctx, TokenMeta{
		Address: "0xtok0", Symbol: "USDe.v2", LogoURL: "https://logo/v2.png", Decimals: intp(6),
	}); err != nil {
		t.Fatalf("re-upsert tok0: %v", err)
	}
	got2, err := db.TokenMetaByAddresses(ctx, []string{"0xtok0"})
	if err != nil {
		t.Fatalf("re-join: %v", err)
	}
	if r := got2["0xtok0"]; r.Symbol != "USDe.v2" || r.LogoURL != "https://logo/v2.png" || r.Decimals == nil || *r.Decimals != 6 {
		t.Fatalf("re-upsert did not replace: %+v", r)
	}

	// Empty address is rejected (no silent no-op).
	if err := db.UpsertTokenMeta(ctx, TokenMeta{Address: ""}); err == nil {
		t.Fatal("empty address: want error, got nil")
	}

	// Empty/blank input -> empty (non-nil) map, no query error.
	if m, err := db.TokenMetaByAddresses(ctx, nil); err != nil || m == nil || len(m) != 0 {
		t.Fatalf("empty input join = (%v,%v), want (empty,nil)", m, err)
	}
	if m, err := db.TokenMetaByAddresses(ctx, []string{"", "   "}); err != nil || len(m) != 0 {
		t.Fatalf("blank-only input join = (%v,%v), want (empty,nil)", m, err)
	}
}

// TestRawLogByRef verifies the (chain_id, tx_hash, log_index) point-lookup behind
// the on-demand swap decode: it round-trips a stored log, lowercases the tx hash on
// lookup, scopes by chain id, and returns (zero,false,nil) for a miss.
func TestRawLogByRef(t *testing.T) {
	db := newClarityTestDB(t)
	ctx := context.Background()

	bt := time.Date(2026, 6, 8, 9, 0, 0, 0, time.UTC)
	if _, err := db.InsertRawLogs(ctx, []RawLog{{
		ChainID: 5000, BlockNumber: 100, BlockTime: bt,
		TxHash: "0xSwapTx", LogIndex: 4, Address: "0xPool",
		Topic0: "0xtopic", Data: "0xdeadbeef",
	}}); err != nil {
		t.Fatalf("insert raw log: %v", err)
	}

	// Hit (mixed-case tx hash in the lookup must match the lowercase-stored row).
	l, found, err := db.RawLogByRef(ctx, 5000, "0xSWAPTX", 4)
	if err != nil || !found {
		t.Fatalf("RawLogByRef hit = (found=%v, err=%v)", found, err)
	}
	if l.TxHash != "0xswaptx" || l.LogIndex != 4 || l.Address != "0xpool" || l.Data != "0xdeadbeef" {
		t.Fatalf("scanned log wrong: %+v", l)
	}
	if !l.BlockTime.Equal(bt) {
		t.Fatalf("block_time = %v, want %v", l.BlockTime, bt)
	}

	// Miss: wrong log index, wrong chain id, and unknown tx all -> not found, no error.
	for _, miss := range []struct {
		chain int64
		tx    string
		idx   int
	}{
		{5000, "0xswaptx", 5},   // wrong index
		{5003, "0xswaptx", 4},   // wrong chain
		{5000, "0xnotexist", 4}, // unknown tx
	} {
		_, found, err := db.RawLogByRef(ctx, miss.chain, miss.tx, miss.idx)
		if err != nil || found {
			t.Errorf("RawLogByRef(%d,%s,%d) = (found=%v, err=%v), want (false,nil)", miss.chain, miss.tx, miss.idx, found, err)
		}
	}
}

// TestRecentLargeSwapsByActor verifies the actor activity-path read: newest-first
// order, the limit bound, signed net0/net1 round-trip (including a NULL-net row read
// as 0), and the non-positive-limit short circuit.
func TestRecentLargeSwapsByActor(t *testing.T) {
	db := newClarityTestDB(t)
	ctx := context.Background()

	base := time.Date(2026, 6, 8, 8, 0, 0, 0, time.UTC)
	mk := func(tx string, idx int, pool string, net0, net1 string, when time.Time) {
		t.Helper()
		const q = `
			INSERT INTO large_swaps (tx_hash, log_index, pool, actor, size_token0, net0, net1, block_time)
			VALUES ($1,$2,$3,'0xactor', '1000', $4, $5, $6)`
		var n0, n1 any
		if net0 == "" {
			n0 = nil
		} else {
			n0 = net0
		}
		if net1 == "" {
			n1 = nil
		} else {
			n1 = net1
		}
		if _, err := db.Pool.Exec(ctx, q, tx, idx, pool, n0, n1, when); err != nil {
			t.Fatalf("insert large_swap %s: %v", tx, err)
		}
	}

	// Three swaps for the actor at increasing times; one with NULL nets.
	mk("0xa", 0, "0xpoolA", "-100", "95", base.Add(1*time.Minute))
	mk("0xb", 1, "0xpoolB", "", "", base.Add(2*time.Minute)) // NULL nets -> read as 0
	mk("0xc", 2, "0xpoolA", "200", "-3", base.Add(3*time.Minute))
	// A different actor's swap must not appear.
	if _, err := db.Pool.Exec(ctx, `INSERT INTO large_swaps (tx_hash, log_index, pool, actor, size_token0, net0, net1, block_time)
		VALUES ('0xz',0,'0xpoolA','0xother','1','1','1',$1)`, base); err != nil {
		t.Fatalf("insert other actor: %v", err)
	}

	got, err := db.RecentLargeSwapsByActor(ctx, "0xACTOR", 25) // mixed-case actor
	if err != nil {
		t.Fatalf("RecentLargeSwapsByActor: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d swaps, want 3 (other actor excluded): %+v", len(got), got)
	}
	// Newest first: 0xc, 0xb, 0xa.
	if got[0].TxHash != "0xc" || got[1].TxHash != "0xb" || got[2].TxHash != "0xa" {
		t.Fatalf("order wrong: %s,%s,%s", got[0].TxHash, got[1].TxHash, got[2].TxHash)
	}
	// Signed nets round-trip; NULL nets read as 0.
	if got[2].Net0.String() != "-100" || got[2].Net1.String() != "95" {
		t.Fatalf("0xa nets wrong: net0=%s net1=%s", got[2].Net0, got[2].Net1)
	}
	if !got[1].Net0.IsZero() || !got[1].Net1.IsZero() {
		t.Fatalf("0xb NULL nets should read as 0: net0=%s net1=%s", got[1].Net0, got[1].Net1)
	}

	// Limit bounds the result to the newest N.
	limited, err := db.RecentLargeSwapsByActor(ctx, "0xactor", 2)
	if err != nil {
		t.Fatalf("limit: %v", err)
	}
	if len(limited) != 2 || limited[0].TxHash != "0xc" || limited[1].TxHash != "0xb" {
		t.Fatalf("limit=2 wrong: %+v", limited)
	}

	// Non-positive limit short-circuits to empty, no query.
	if none, err := db.RecentLargeSwapsByActor(ctx, "0xactor", 0); err != nil || len(none) != 0 {
		t.Fatalf("limit 0 = (%v,%v), want (empty,nil)", none, err)
	}
}
