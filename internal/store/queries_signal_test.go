package store

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testDBEnv is the env var holding a Postgres DSN for the store integration tests. When
// unset, the DB-dependent tests skip (so `go test ./...` stays green with no Postgres).
// Point it at a throwaway database, e.g.:
//
//	OCR_TEST_DATABASE_URL=postgres://user:pass@localhost:5432/ocr_test go test ./internal/store
//
// The tests are non-destructive: each creates its own randomly-named schema, works only
// inside it, and drops it on cleanup, never touching the public schema or live tables.
const testDBEnv = "OCR_TEST_DATABASE_URL"

// newTestDB opens a schema-isolated *DB against OCR_TEST_DATABASE_URL, or skips
// the test when the env var is unset. It creates a fresh schema, pins every
// pooled connection to it via search_path, and registers cleanup that drops the
// schema and closes the pool. The returned DB behaves exactly like a production
// DB but reads/writes only the throwaway schema.
func newTestDB(t *testing.T) *DB {
	t.Helper()

	dsn := os.Getenv(testDBEnv)
	if dsn == "" {
		t.Skipf("%s not set; skipping store integration test", testDBEnv)
	}

	schema := fmt.Sprintf("ocr_test_%d_%d", time.Now().UnixNano(), rand.Intn(1_000_000)) //nolint:gosec // test-only schema name, not security-sensitive

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	// Pin the whole pool to the throwaway schema so every query runs against it.
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema

	ctx := context.Background()

	// The schema must exist before pooled connections set search_path to it, so
	// create it on a separate bootstrap connection that does not pin search_path.
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

	db := &DB{Pool: pool}

	// Create just the signals DDL the queries under test touch: the base columns
	// (0001_init.sql) plus the later-added event_ref (0002), tg_message_id (0005),
	// actor (0008), and size_usd (0011). Kept in lock-step with signalColumns /
	// scanSignals.
	const ddl = `
		CREATE TABLE signals (
			id             BIGSERIAL PRIMARY KEY,
			created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			bucket_ts      TIMESTAMPTZ,
			pool           TEXT        NOT NULL,
			signal_type    SMALLINT    NOT NULL,
			metric         TEXT        NOT NULL,
			zscore         NUMERIC     NOT NULL,
			window_buckets INTEGER     NOT NULL,
			payload        JSONB       NOT NULL DEFAULT '{}'::jsonb,
			llm_note       TEXT,
			attest_tx      TEXT,
			status         TEXT        NOT NULL DEFAULT 'new',
			event_ref      TEXT,
			tg_message_id  TEXT,
			actor          TEXT,
			size_usd       NUMERIC
		)`
	if _, err := pool.Exec(ctx, ddl); err != nil {
		pool.Close()
		dropSchema(t, bootCfg.ConnString(), schema)
		t.Fatalf("create signals table: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		dropSchema(t, bootCfg.ConnString(), schema)
	})
	return db
}

// dropSchema removes the throwaway schema on a fresh connection (the pool is
// already closed by the time cleanup runs).
func dropSchema(t *testing.T, connString, schema string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, connString)
	if err != nil {
		t.Logf("cleanup: connect to drop schema %s: %v", schema, err)
		return
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
		t.Logf("cleanup: drop schema %s: %v", schema, err)
	}
}

// insertTestSignal inserts one signal directly (bypassing InsertSignal's
// idempotency/canonicalization) so the test controls llm_note and created_at
// exactly. createdAt orders the rows for the newest-first assertion; llmNote is
// passed as *string so a nil pointer stores SQL NULL.
func insertTestSignal(t *testing.T, db *DB, pool string, createdAt time.Time, llmNote *string) int64 {
	t.Helper()
	const q = `
		INSERT INTO signals
			(created_at, pool, signal_type, metric, zscore, window_buckets, llm_note, status)
		VALUES ($1,$2,1,'vol0',5.0,576,$3,'new')
		RETURNING id`
	var id int64
	if err := db.Pool.QueryRow(context.Background(), q, createdAt, pool, llmNote).Scan(&id); err != nil {
		t.Fatalf("insert test signal: %v", err)
	}
	return id
}

// TestSignalsNeedingNote verifies the async-enrich work queue: it must return
// only signals whose llm_note is NULL or empty/whitespace, newest-first, bounded
// by the limit, and never a signal that already has a real note.
func TestSignalsNeedingNote(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	base := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	realNote := "Volume spiked. Likely arb. medium"
	blank := ""
	whitespace := "   \n\t "

	// Insert in a deliberately scrambled created_at order; the query must still
	// return the note-less ones newest-first.
	idNull := insertTestSignal(t, db, "0xpoolnull", base.Add(4*time.Minute), nil)             // newest, NULL note  -> included
	idWhitespace := insertTestSignal(t, db, "0xpoolws", base.Add(3*time.Minute), &whitespace) // whitespace-only    -> included
	_ = insertTestSignal(t, db, "0xpoolnote", base.Add(2*time.Minute), &realNote)             // has a real note     -> excluded
	idBlank := insertTestSignal(t, db, "0xpoolblank", base.Add(1*time.Minute), &blank)        // empty string        -> included

	got, err := db.SignalsNeedingNote(ctx, 50)
	if err != nil {
		t.Fatalf("SignalsNeedingNote: %v", err)
	}

	// Exactly the three note-less signals, newest created_at first.
	wantOrder := []int64{idNull, idWhitespace, idBlank}
	if len(got) != len(wantOrder) {
		t.Fatalf("got %d signals, want %d: %+v", len(got), len(wantOrder), ids(got))
	}
	for i, want := range wantOrder {
		if got[i].ID != want {
			t.Errorf("position %d: got id %d, want %d (full order %v, want %v)", i, got[i].ID, want, ids(got), wantOrder)
		}
		// Each returned signal must genuinely need a note: NULL (scanned as "")
		// or all-whitespace, mirroring the query's strings.TrimSpace semantics.
		if strings.TrimSpace(got[i].LLMNote) != "" {
			t.Errorf("signal %d returned with a real note %q; it should need a note", got[i].ID, got[i].LLMNote)
		}
	}

	// The limit must bound the batch: with limit 2 we get the two newest only.
	limited, err := db.SignalsNeedingNote(ctx, 2)
	if err != nil {
		t.Fatalf("SignalsNeedingNote(limit=2): %v", err)
	}
	if len(limited) != 2 || limited[0].ID != idNull || limited[1].ID != idWhitespace {
		t.Errorf("limit=2: got %v, want [%d %d]", ids(limited), idNull, idWhitespace)
	}

	// A non-positive limit short-circuits to an empty result (no query).
	if none, err := db.SignalsNeedingNote(ctx, 0); err != nil || len(none) != 0 {
		t.Errorf("SignalsNeedingNote(0) = (%v, %v), want (empty, nil)", ids(none), err)
	}
}

// TestUpdateSignalTGMessageID verifies the message-id write: it round-trips the
// id, rejects an empty id, and errors on a missing signal (never a silent no-op).
func TestUpdateSignalTGMessageID(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	id := insertTestSignal(t, db, "0xpool", time.Now().UTC(), nil)

	if err := db.UpdateSignalTGMessageID(ctx, id, "4242"); err != nil {
		t.Fatalf("UpdateSignalTGMessageID: %v", err)
	}

	// It must round-trip onto the scanned Signal (via COALESCE in signalColumns).
	sigs, err := db.SignalsNeedingNote(ctx, 10)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	var found *Signal
	for i := range sigs {
		if sigs[i].ID == id {
			found = &sigs[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("signal %d not found after update", id)
	}
	if found.TGMessageID != "4242" {
		t.Errorf("TGMessageID = %q, want %q", found.TGMessageID, "4242")
	}

	// Empty id is rejected without touching the DB.
	if err := db.UpdateSignalTGMessageID(ctx, id, ""); err == nil {
		t.Error("empty message id: want error, got nil")
	}

	// A missing signal must error, not silently affect zero rows.
	if err := db.UpdateSignalTGMessageID(ctx, 999999, "1"); err == nil {
		t.Error("missing signal: want error, got nil")
	}
}

// ids extracts the id slice from signals for readable assertion failures.
func ids(sigs []Signal) []int64 {
	out := make([]int64, len(sigs))
	for i := range sigs {
		out[i] = sigs[i].ID
	}
	return out
}
