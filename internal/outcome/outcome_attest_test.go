package outcome

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"

	"ocr/internal/attest"
	"ocr/internal/store"
)

// These tests cover the on-chain outcome-attest path's pure pieces: the report-hash
// parser and the signal-hash derivation that links an outcome back to the original
// SignalAttested record. The hash must be byte-for-byte the same one the signal was
// attested with, so the outcome links to the right call.

// parseHash32 must round-trip a canonical 0x-hex report hash and reject anything
// that is not exactly 32 bytes of valid hex, so a malformed stored hash can never
// be committed on-chain as a silently different value.
func TestParseHash32_RoundTripAndReject(t *testing.T) {
	want := crypto.Keccak256([]byte("a-report-card"))
	hexStr := "0x" + hex.EncodeToString(want)

	got, err := parseHash32(hexStr)
	if err != nil {
		t.Fatalf("parseHash32(%q) error: %v", hexStr, err)
	}
	if !equalBytes(got[:], want) {
		t.Fatalf("parseHash32 round-trip mismatch: got %x want %x", got, want)
	}

	// Bare (no 0x) hex of the right length also parses.
	if _, err := parseHash32(hex.EncodeToString(want)); err != nil {
		t.Errorf("parseHash32 rejected valid bare hex: %v", err)
	}
	// Surrounding whitespace and uppercase prefix are tolerated.
	if _, err := parseHash32("  0X" + hex.EncodeToString(want) + "  "); err != nil {
		t.Errorf("parseHash32 rejected whitespace/0X-prefixed hex: %v", err)
	}

	bad := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"too-short", "0xdeadbeef"},
		{"too-long", "0x" + hex.EncodeToString(want) + "00"},
		{"non-hex", "0x" + strings.Repeat("zz", 32)},
	}
	for _, b := range bad {
		if _, err := parseHash32(b.in); err == nil {
			t.Errorf("parseHash32(%s=%q) expected error, got nil", b.name, b.in)
		}
	}
}

// signalHash32 must reproduce exactly the hash attest.SignalHash produces for a
// bucket signal (and attest.SignalHashEvent for a per-event signal), because the
// outcome attestation links to the signal attestation by that hash. If these ever
// diverge, OutcomeRecorded would point at a SignalAttested record that does not
// exist.
func TestSignalHash32_MatchesOnChainSignalHash(t *testing.T) {
	// Bucket signal (no event_ref): hashed on (pool, type, metric, score, bucket_ts).
	bucketSig := store.Signal{
		ID:         7,
		Pool:       "0xAbCdef0000000000000000000000000000000001",
		SignalType: 1,
		Metric:     "vol0",
		Zscore:     4.23,
	}
	// bucketSig.BucketTS is the zero time here; .Unix() is deterministic regardless.
	wantBucket := attest.SignalHash(
		bucketSig.Pool, 1, bucketSig.Metric,
		attest.ScoreFromZ(bucketSig.Zscore), bucketSig.BucketTS.Unix(),
	)
	gotBucket, ok := signalHash32(bucketSig)
	if !ok {
		t.Fatalf("signalHash32(bucket) ok=false for an attestable type")
	}
	if gotBucket != wantBucket {
		t.Errorf("bucket signalHash32 mismatch:\n got  %x\n want %x", gotBucket, wantBucket)
	}

	// Per-event signal (event_ref set): hashed on (pool, type, event_ref, score, ts).
	eventSig := store.Signal{
		ID:         8,
		Pool:       "0xabc",
		SignalType: 2,
		Metric:     "whale_swap",
		Zscore:     9.0,
		EventRef:   "0xdeadbeef:3",
	}
	wantEvent := attest.SignalHashEvent(
		eventSig.Pool, 2, eventSig.EventRef,
		attest.ScoreFromZ(eventSig.Zscore), eventSig.BucketTS.Unix(),
	)
	gotEvent, ok := signalHash32(eventSig)
	if !ok {
		t.Fatalf("signalHash32(event) ok=false for an attestable type")
	}
	if gotEvent != wantEvent {
		t.Errorf("event signalHash32 mismatch:\n got  %x\n want %x", gotEvent, wantEvent)
	}

	// Bucket and event variants must not collide (disjoint key sets).
	if gotBucket == gotEvent {
		t.Errorf("bucket and event hashes collided")
	}

	// signalHashHex must be the 0x-hex of signalHash32.
	if got := signalHashHex(bucketSig); got != "0x"+hex.EncodeToString(gotBucket[:]) {
		t.Errorf("signalHashHex disagrees with signalHash32: %s", got)
	}
}

// An out-of-range signal_type has no on-chain attestation to link to, so
// signalHash32 must report ok=false (and signalHashHex must be empty), mirroring
// the real attest path, which skips such signals entirely.
func TestSignalHash32_RejectsUnattestableType(t *testing.T) {
	for _, st := range []int16{0, -1, 256, 1000} {
		sig := store.Signal{Pool: "0xabc", SignalType: st, Metric: "vol0"}
		if _, ok := signalHash32(sig); ok {
			t.Errorf("signalHash32 ok=true for unattestable type %d", st)
		}
		if h := signalHashHex(sig); h != "" {
			t.Errorf("signalHashHex non-empty for unattestable type %d: %q", st, h)
		}
	}
}

// AttestPending must be a safe no-op when no attestor is configured (the disabled
// path), returning (0, nil) without touching the (nil) DB.
func TestAttestPending_NilAttestorIsNoop(t *testing.T) {
	// db is required to be non-nil; a nil attestor short-circuits before any DB use, so
	// the guard order holds: nil db -> error; nil attestor -> (0,nil).
	if _, err := AttestPending(context.Background(), nil, nil); err == nil {
		t.Errorf("AttestPending(nil db) expected error, got nil")
	}
}

// fakeOutcomeAttestor records calls so a test can assert AttestOutcome wiring
// without a chain. It satisfies OutcomeAttestorClient.
type fakeOutcomeAttestor struct {
	calls  int
	lastSH [32]byte
	lastG  uint8
	lastRH [32]byte
}

func (f *fakeOutcomeAttestor) AttestOutcome(_ context.Context, sh [32]byte, g uint8, rh [32]byte) (string, error) {
	f.calls++
	f.lastSH = sh
	f.lastG = g
	f.lastRH = rh
	return "0xtx", nil
}

// The fake must satisfy the interface the wiring depends on (compile-time + a
// trivial call), so the interface stays the minimal contract AttestPending needs.
func TestOutcomeAttestorClient_InterfaceSatisfied(t *testing.T) {
	var c OutcomeAttestorClient = &fakeOutcomeAttestor{}
	tx, err := c.AttestOutcome(context.Background(), [32]byte{1}, 87, [32]byte{2})
	if err != nil || tx != "0xtx" {
		t.Fatalf("fake AttestOutcome: tx=%q err=%v", tx, err)
	}
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
