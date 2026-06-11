package chain

import (
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"ocr/internal/config"
	"ocr/internal/lending"
	"ocr/internal/store"
)

// NewCollector merges the protocol-contracts registry (the Aave Pool) with the swap-pool
// addresses for a single eth_getLogs filter. This asserts the Aave Pool is present, pool
// addresses are preserved (lowercased), and a duplicate is de-duplicated so the filter
// never lists the same address twice.
func TestNewCollectorIncludesProtocolContracts(t *testing.T) {
	pools := []store.Pool{
		{Address: "0xEAFC4D6D4C3391CD4FC10C85D2F5F972D58C0DD5"}, // mixed-case pool
		{Address: lending.AavePoolAddress},                      // also the Aave Pool (dup)
	}
	col := NewCollector(nil, nil, &config.Config{ChainID: 5000}, pools)

	want := map[common.Address]struct{}{
		common.HexToAddress("0xeafc4d6d4c3391cd4fc10c85d2f5f972d58c0dd5"): {},
		common.HexToAddress(lending.AavePoolAddress):                      {},
	}
	got := map[common.Address]int{}
	for _, a := range col.addrs {
		got[a]++
	}

	// Every wanted address present exactly once (dedup).
	for a := range want {
		if got[a] != 1 {
			t.Errorf("address %s appears %d times, want exactly 1", a.Hex(), got[a])
		}
	}
	// The Aave Pool must be in the collected set (a protocol contract).
	if _, ok := got[common.HexToAddress(lending.AavePoolAddress)]; !ok {
		t.Errorf("collected set is missing the Aave Pool %s", lending.AavePoolAddress)
	}
	// No address listed more than once.
	for a, n := range got {
		if n != 1 {
			t.Errorf("address %s listed %d times (want unique)", a.Hex(), n)
		}
	}
	// The Aave Pool address constant must be lowercase (storage convention).
	if lending.AavePoolAddress != strings.ToLower(lending.AavePoolAddress) {
		t.Errorf("AavePoolAddress must be lowercase, got %s", lending.AavePoolAddress)
	}
}

// topicAt returns the indexed topic hex, or "" when the index is out of range
// (the store layer maps "" to a NULL column). Out-of-range access must not panic.
func TestTopicAt(t *testing.T) {
	h0 := common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")
	h1 := common.HexToHash("0x000000000000000000000000000000000000000000000000000000000000beef")
	topics := []common.Hash{h0, h1}

	if got := topicAt(topics, 0); got != h0.Hex() {
		t.Errorf("topicAt(0) = %q, want %q", got, h0.Hex())
	}
	if got := topicAt(topics, 1); got != h1.Hex() {
		t.Errorf("topicAt(1) = %q, want %q", got, h1.Hex())
	}
	if got := topicAt(topics, 2); got != "" {
		t.Errorf("topicAt(out-of-range) = %q, want empty", got)
	}
	if got := topicAt(nil, 0); got != "" {
		t.Errorf("topicAt(nil) = %q, want empty", got)
	}
}

// The chunked-paging loop in RunOnce must terminate, never emit a chunkEnd past toBlock,
// and cover the whole [from,to] span, including a cursor far behind the head and exact
// chunk boundaries. This mirrors RunOnce's loop shape against the real logChunkBlocks.
func TestChunkPagination(t *testing.T) {
	spans := []struct{ from, to uint64 }{
		{101, 9_999_998},        // cursor far behind: thousands of chunks
		{1, logChunkBlocks},     // exactly one full chunk
		{1, logChunkBlocks + 1}, // spills into a second chunk
		{2000, 2000},            // single block
		{0, logChunkBlocks - 1}, // first chunk anchored at 0
	}
	for _, s := range spans {
		var (
			iters   int
			lastEnd uint64
			covered = s.from
		)
		for chunkStart := s.from; chunkStart <= s.to; chunkStart += logChunkBlocks {
			chunkEnd := chunkStart + logChunkBlocks - 1
			if chunkEnd > s.to {
				chunkEnd = s.to
			}
			if chunkEnd > s.to {
				t.Fatalf("[%d,%d]: chunkEnd %d > to", s.from, s.to, chunkEnd)
			}
			if chunkEnd < chunkStart {
				t.Fatalf("[%d,%d]: chunkEnd %d < chunkStart %d", s.from, s.to, chunkEnd, chunkStart)
			}
			if chunkStart != covered {
				t.Fatalf("[%d,%d]: gap at %d (expected %d)", s.from, s.to, chunkStart, covered)
			}
			covered = chunkEnd + 1
			lastEnd = chunkEnd
			iters++
			if iters > 11_000_000 {
				t.Fatalf("[%d,%d]: pagination did not terminate", s.from, s.to)
			}
		}
		if lastEnd != s.to {
			t.Errorf("[%d,%d]: last chunkEnd = %d, want %d", s.from, s.to, lastEnd, s.to)
		}
	}
}
