package api

import (
	"sync"

	"github.com/rs/zerolog/log"
)

// Hub is the SSE pub/sub fan-out for the live signal feed, decoupling the producer (a
// DB-tail goroutine) from the consumers (connected SSE clients): the producer calls
// Broadcast; each client has its own buffered channel and reads at its own pace.
//
// Stability invariants:
//   - Broadcast never blocks on a slow client: it does a non-blocking send, and a
//     client whose buffer is full is dropped (channel closed) rather than stalling the
//     others or the producer. The browser EventSource reconnects and replays via
//     Last-Event-ID, so a transient slow reader self-heals.
//   - All shared state is guarded by one mutex; a plain map + mutex is the simplest
//     correct design at this scale (a handful of dashboard viewers).
//
// The detector / attestor know nothing about the Hub (it is fed by the DB tail), so the
// live feed is restart-safe and not coupled to the hot path.
type Hub struct {
	mu      sync.Mutex
	clients map[*client]struct{}
	closed  bool
}

// client is one connected SSE subscriber. ch is its private buffered delivery channel,
// which the handler goroutine ranges over. The Hub owns ch's lifecycle: it is closed
// exactly once, by the Hub, when the client is unregistered or dropped.
type client struct {
	ch chan sseEvent
}

// sseEvent is one event queued for delivery: a pre-serialized signal plus its id
// (used as the SSE event id for Last-Event-ID reconnection) and event name.
type sseEvent struct {
	id   int64  // signal id; becomes the SSE "id:" line
	name string // SSE "event:" name, e.g. "signal"
	data []byte // pre-marshaled JSON payload (the SignalDTO)
}

// clientBufferSize is the per-client queue depth. A client this many events behind is
// dropped rather than blocking the broadcaster. 64 is generous for a UI that animates one
// event at a time; a reader that cannot keep up is wedged and better off reconnecting.
const clientBufferSize = 64

// maxSSEClients caps concurrent SSE clients: the Hub holds one channel per client and
// Broadcast is O(clients) under a lock, so the fan-out is bounded for memory and latency
// on this public endpoint. A connection beyond this is refused with 503 (the slow-client
// drop still frees slots). 256 is far above any realistic dashboard audience.
const maxSSEClients = 256

// NewHub constructs an empty Hub ready to accept subscribers.
func NewHub() *Hub {
	return &Hub{clients: make(map[*client]struct{})}
}

// subscribe registers a new client and returns it. The caller must defer unsubscribe(c)
// so the Hub frees the slot and closes the channel exactly once. Subscribing to a closed
// Hub returns a client whose channel is already closed, so the handler's range loop exits
// immediately. It returns (nil, false) at maxSSEClients capacity, where the caller must
// refuse with 503; Broadcast's slow-client drop makes capacity self-healing.
func (h *Hub) subscribe() (*client, bool) {
	c := &client{ch: make(chan sseEvent, clientBufferSize)}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		close(c.ch)
		return c, true
	}
	if len(h.clients) >= maxSSEClients {
		// At capacity: the caller responds 503; the channel is never used, no close.
		return nil, false
	}
	h.clients[c] = struct{}{}
	log.Debug().Int("clients", len(h.clients)).Msg("api: sse client subscribed")
	return c, true
}

// unsubscribe removes a client and closes its channel exactly once. The membership check
// makes it safe for a client already dropped by Broadcast, so the handler can always
// defer unsubscribe without racing the drop path.
func (h *Hub) unsubscribe(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[c]; !ok {
		// Already removed (dropped by Broadcast); its channel was closed there.
		return
	}
	delete(h.clients, c)
	close(c.ch)
	log.Debug().Int("clients", len(h.clients)).Msg("api: sse client unsubscribed")
}

// Broadcast fans one event out to every subscribed client without ever blocking: each
// delivery is a non-blocking select, and a client whose buffer is full is dropped
// (channel closed) so one slow reader cannot stall the others. Returns the number of
// clients the event was queued to (after drops).
func (h *Hub) Broadcast(ev sseEvent) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0
	}
	delivered := 0
	for c := range h.clients {
		select {
		case c.ch <- ev:
			delivered++
		default:
			// Buffer full: drop this client (the browser reconnects and replays via
			// Last-Event-ID). Closing + deleting here makes a later unsubscribe a no-op.
			delete(h.clients, c)
			close(c.ch)
			log.Warn().Int64("event_id", ev.id).Msg("api: dropped slow sse client (buffer full)")
		}
	}
	return delivered
}

// ClientCount returns the number of currently-subscribed clients (for
// observability / tests).
func (h *Hub) ClientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// Close shuts the Hub down: marks it closed (future Broadcast/subscribe are no-ops) and
// closes every live client channel so their handler range loops exit. Idempotent; called
// on server stop so no SSE goroutine leaks past shutdown.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for c := range h.clients {
		delete(h.clients, c)
		close(c.ch)
	}
	log.Debug().Msg("api: sse hub closed")
}
