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

// DB-gated round-trip test for the pools registry, focused on the nullable bin_step
// column (migration 0012): UpsertPool persists a *int (nil -> NULL, value -> INT) and
// ListEnabledPools scans it back nil-safe. Skips when OCR_TEST_DATABASE_URL is unset;
// runs in its own randomly-named throwaway schema dropped on cleanup; same isolation
// contract as queries_signal_test.go.

// newPoolTestDB opens a schema-isolated *DB and creates a pools table that includes
// every column UpsertPool / ListEnabledPools touch, including the nullable tuning
// overrides (0004) and bin_step (0012). Kept minimal and in lock-step with those queries.
func newPoolTestDB(t *testing.T) *DB {
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
			whale_min_multiple DOUBLE PRECISION,
			bin_step           INT
		);`
	if _, err := pool.Exec(ctx, ddl); err != nil {
		pool.Close()
		dropSchema(t, bootCfg.ConnString(), schema)
		t.Fatalf("create pools table: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		dropSchema(t, bootCfg.ConnString(), schema)
	})
	return &DB{Pool: pool}
}

// TestUpsertPool_BinStepRoundTrip verifies bin_step round-trips through UpsertPool +
// ListEnabledPools: a moe pool with a value scans back as that value, an agni pool
// with nil scans back as nil, and a later upsert that SETS a previously-nil bin step
// (the `ocr poolmeta` backfill shape) is persisted.
func TestUpsertPool_BinStepRoundTrip(t *testing.T) {
	db := newPoolTestDB(t)
	ctx := context.Background()

	bs := 25
	// A merchant_moe pool with a known bin step, and an agni pool with bin_step nil.
	if err := db.UpsertPool(ctx, Pool{
		Address: "0xMoe", Dex: "merchant_moe", Token0: "0xT0", Token1: "0xT1",
		Dec0: 8, Dec1: 18, Label: "FBTC/cmETH", Enabled: true, BinStep: &bs,
	}); err != nil {
		t.Fatalf("upsert moe pool: %v", err)
	}
	if err := db.UpsertPool(ctx, Pool{
		Address: "0xAgni", Dex: "agni", Token0: "0xA0", Token1: "0xA1",
		Dec0: 18, Dec1: 18, Label: "USDe/WMNT", Enabled: true, BinStep: nil,
	}); err != nil {
		t.Fatalf("upsert agni pool: %v", err)
	}

	pools, err := db.ListEnabledPools(ctx)
	if err != nil {
		t.Fatalf("list enabled pools: %v", err)
	}
	got := map[string]*int{}
	for _, p := range pools {
		got[p.Address] = p.BinStep
	}

	moeBS, ok := got["0xmoe"] // addresses are stored lowercase
	if !ok {
		t.Fatalf("moe pool missing from enabled list: %+v", got)
	}
	if moeBS == nil || *moeBS != 25 {
		t.Fatalf("moe bin_step = %v, want 25", moeBS)
	}
	agniBS, ok := got["0xagni"]
	if !ok {
		t.Fatalf("agni pool missing from enabled list: %+v", got)
	}
	if agniBS != nil {
		t.Fatalf("agni bin_step = %v, want nil", *agniBS)
	}

	// Backfill shape: re-upsert the agni pool WITH a bin step (as `ocr poolmeta`
	// would) and confirm the UPDATE persists it.
	abs := 5
	agni := Pool{
		Address: "0xAgni", Dex: "agni", Token0: "0xA0", Token1: "0xA1",
		Dec0: 18, Dec1: 18, Label: "USDe/WMNT", Enabled: true, BinStep: &abs,
	}
	if err := db.UpsertPool(ctx, agni); err != nil {
		t.Fatalf("re-upsert agni with bin step: %v", err)
	}
	if bs := binStepOf(t, db, "0xagni"); bs == nil || *bs != 5 {
		t.Fatalf("agni bin_step after backfill = %v, want 5", bs)
	}

	// preserve-on-null: re-upsert the moe pool without a bin step (exactly the
	// `seedPools` YAML-registry shape, BinStep nil). The previously-backfilled value
	// (25) must survive: a nil upsert must never clobber it back to NULL, or every
	// supervisor restart would wipe the YAML moe pools' bin steps and break lp_url.
	moeNoBS := Pool{
		Address: "0xMoe", Dex: "merchant_moe", Token0: "0xT0", Token1: "0xT1",
		Dec0: 8, Dec1: 18, Label: "FBTC/cmETH", Enabled: true, BinStep: nil,
	}
	if err := db.UpsertPool(ctx, moeNoBS); err != nil {
		t.Fatalf("re-upsert moe without bin step (seed shape): %v", err)
	}
	if bs := binStepOf(t, db, "0xmoe"); bs == nil || *bs != 25 {
		t.Fatalf("moe bin_step after a nil re-upsert = %v, want 25 (preserved)", bs)
	}
}

// binStepOf reads one enabled pool's bin_step by lowercase address for the assertions.
func binStepOf(t *testing.T, db *DB, addr string) *int {
	t.Helper()
	pools, err := db.ListEnabledPools(context.Background())
	if err != nil {
		t.Fatalf("list enabled pools: %v", err)
	}
	for _, p := range pools {
		if p.Address == addr {
			return p.BinStep
		}
	}
	t.Fatalf("pool %s not found in enabled list", addr)
	return nil
}
