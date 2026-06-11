package api

import (
	"testing"
)

// The Hub is the SSE fan-out and the stability-critical piece, so these hermetic
// tests (no DB, no HTTP) pin its contract: deliver to all subscribers, never block
// on a slow client (drop it instead), and clean up channels exactly once.

// TestHubBroadcastDelivers verifies a broadcast reaches every subscribed client
// and the event arrives intact.
func TestHubBroadcastDelivers(t *testing.T) {
	h := NewHub()
	c1, _ := h.subscribe()
	c2, _ := h.subscribe()
	if got := h.ClientCount(); got != 2 {
		t.Fatalf("ClientCount = %d, want 2", got)
	}

	ev := sseEvent{id: 7, name: eventSignal, data: []byte(`{"id":7}`)}
	delivered := h.Broadcast(ev)
	if delivered != 2 {
		t.Fatalf("Broadcast delivered = %d, want 2", delivered)
	}

	for i, c := range []*client{c1, c2} {
		select {
		case got := <-c.ch:
			if got.id != 7 || got.name != eventSignal || string(got.data) != `{"id":7}` {
				t.Fatalf("client %d got %+v, want id 7 signal", i, got)
			}
		default:
			t.Fatalf("client %d received nothing", i)
		}
	}
}

// TestHubDropsSlowClient verifies the broadcaster never blocks on a full client
// buffer: a client that does not drain is dropped (its channel closed, removed
// from the set) while other clients keep receiving. This is the anti-stall
// guarantee.
func TestHubDropsSlowClient(t *testing.T) {
	h := NewHub()
	slow, _ := h.subscribe() // never drained
	fast, _ := h.subscribe()

	// Fill the slow client's buffer to capacity so the next broadcast cannot queue
	// to it (forcing a drop), but keep draining the fast client.
	for i := 0; i < clientBufferSize; i++ {
		h.Broadcast(sseEvent{id: int64(i), name: eventSignal, data: []byte("x")})
		// Drain the fast client each iteration so it never fills up.
		select {
		case <-fast.ch:
		default:
		}
	}

	// The slow client is now full; this broadcast must drop it (not block).
	before := h.ClientCount()
	if before != 2 {
		t.Fatalf("before drop ClientCount = %d, want 2", before)
	}
	done := make(chan int, 1)
	go func() { done <- h.Broadcast(sseEvent{id: 999, name: eventSignal, data: []byte("y")}) }()

	// Drain the fast client so it accepts the final event; the slow one is dropped.
	select {
	case <-fast.ch:
	default:
	}

	// Broadcast must return promptly (it never blocks); if this hangs the test
	// times out, which is itself the failure signal.
	<-done

	if got := h.ClientCount(); got != 1 {
		t.Fatalf("after drop ClientCount = %d, want 1 (slow client dropped)", got)
	}
	// The dropped client's channel must be closed.
	if _, open := <-slow.ch; open {
		// Drain remaining buffered items until closed; ensure it eventually closes.
		for range slow.ch {
		}
	}
	// A second unsubscribe of the dropped client must be a safe no-op (no panic /
	// double close).
	h.unsubscribe(slow)
}

// TestHubCapacityRefusesAndRecovers verifies the Hub caps concurrent subscribers at
// maxSSEClients (returning ok=false past the cap) and that freeing a slot (via
// unsubscribe or the slow-client drop) lets a new subscriber in again.
func TestHubCapacityRefusesAndRecovers(t *testing.T) {
	h := NewHub()
	subs := make([]*client, 0, maxSSEClients)
	for i := 0; i < maxSSEClients; i++ {
		c, ok := h.subscribe()
		if !ok {
			t.Fatalf("subscribe %d should succeed (under cap)", i)
		}
		subs = append(subs, c)
	}
	if got := h.ClientCount(); got != maxSSEClients {
		t.Fatalf("ClientCount = %d, want %d", got, maxSSEClients)
	}

	// One past capacity must be refused (nil client, ok=false) and must not register.
	if c, ok := h.subscribe(); ok || c != nil {
		t.Fatalf("subscribe past capacity = (%v, %v), want (nil, false)", c, ok)
	}
	if got := h.ClientCount(); got != maxSSEClients {
		t.Fatalf("ClientCount after refusal = %d, want %d (refused client must not register)", got, maxSSEClients)
	}

	// Free one slot; a new subscribe must now succeed.
	h.unsubscribe(subs[0])
	c, ok := h.subscribe()
	if !ok || c == nil {
		t.Fatal("subscribe after freeing a slot should succeed")
	}
}

// TestHubUnsubscribeIdempotent verifies unsubscribe closes the channel once and a
// repeat call is a no-op.
func TestHubUnsubscribeIdempotent(t *testing.T) {
	h := NewHub()
	c, _ := h.subscribe()
	h.unsubscribe(c)
	if _, open := <-c.ch; open {
		t.Fatal("channel should be closed after unsubscribe")
	}
	// Repeat: must not panic (the membership guard makes it a no-op).
	h.unsubscribe(c)
}

// TestHubCloseClosesAll verifies Close closes every client channel and makes
// subsequent subscribe/broadcast no-ops.
func TestHubCloseClosesAll(t *testing.T) {
	h := NewHub()
	c, _ := h.subscribe()
	h.Close()

	if _, open := <-c.ch; open {
		t.Fatal("client channel should be closed after Hub.Close")
	}
	// Broadcast after close delivers to nobody.
	if got := h.Broadcast(sseEvent{id: 1, name: eventSignal, data: []byte("z")}); got != 0 {
		t.Fatalf("Broadcast after Close delivered = %d, want 0", got)
	}
	// Subscribe after close returns an already-closed channel (handler exits).
	c2, _ := h.subscribe()
	if _, open := <-c2.ch; open {
		t.Fatal("subscribe after Close should return a closed channel")
	}
	// Close is idempotent.
	h.Close()
}
