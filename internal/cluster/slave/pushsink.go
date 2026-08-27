package slave

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/scheduler"
	"github.com/tumult/gosmokeping/internal/slavehealth"
)

// PushSink is the only scheduler.Sink the slave's scheduler runs against. It
// buffers every cycle in a fixed-size ring; the runner drains and ships the
// ring on its push cadence. When the buffer is full we drop the *oldest*
// cycle — the freshest data is usually the most operationally interesting.
type PushSink struct {
	log *slog.Logger
	mu  sync.Mutex
	// buf is a ring buffer. head/size index into it; cap is the configured
	// capacity (600 from config, see plan §1/Queueing).
	buf     []cluster.CyclePayload
	head    int
	size    int
	cap     int
	dropped int

	// hopMarkers mirrors the master's advertisement, read on every cycle
	// rather than captured, so a master downgrade takes effect at the next
	// config pull. Zero value false is the fail-closed state a slave holds
	// before its first pull.
	hopMarkers atomic.Bool
}

// NewPushSink constructs a ring-buffered sink. capacity is typically 600
// (≥15× the operator's tolerated master-downtime window).
func NewPushSink(log *slog.Logger, capacity int) *PushSink {
	if capacity <= 0 {
		capacity = 600
	}
	return &PushSink{
		log: log,
		buf: make([]cluster.CyclePayload, capacity),
		cap: capacity,
	}
}

// SetHopMarkers records whether the master understands the TargetReply hop
// marker, from its /config advertisement.
func (p *PushSink) SetHopMarkers(ok bool) { p.hopMarkers.Store(ok) }

// OnCycle implements scheduler.Sink. Serializes the cycle via FromCycle so the
// drain path just ships a batch without touching domain types.
//
// A health target's hops are withheld from a master that redacts by position:
// this walk stops each round at its own terminal, so the slave's own echo can
// sit at ttl 2 under a silent ttl 30, and a positional rule would blank the
// silent row and serve the address.
func (p *PushSink) OnCycle(_ context.Context, c scheduler.Cycle) {
	if slavehealth.IsHealthGroup(c.Target.Group) && !p.hopMarkers.Load() {
		c.Hops = nil
	}
	payload := cluster.FromCycle(c)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.size == p.cap {
		// Overwrite head (oldest).
		p.buf[p.head] = payload
		p.head = (p.head + 1) % p.cap
		p.dropped++
		if p.dropped%100 == 0 {
			p.log.Warn("push buffer full, dropping oldest cycles", "dropped", p.dropped)
		}
		return
	}
	idx := (p.head + p.size) % p.cap
	p.buf[idx] = payload
	p.size++
}

// Drain returns up to max payloads and removes them from the buffer. Caller
// owns the slice. Returns nil/empty when the buffer is empty.
func (p *PushSink) Drain(max int) []cluster.CyclePayload {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.size == 0 {
		return nil
	}
	n := p.size
	if max > 0 && max < n {
		n = max
	}
	out := make([]cluster.CyclePayload, n)
	for i := 0; i < n; i++ {
		out[i] = p.buf[(p.head+i)%p.cap]
	}
	p.head = (p.head + n) % p.cap
	p.size -= n
	return out
}

// Requeue pushes a failed batch back onto the head of the ring. Used when a
// push errors with a retryable status; keeps ordering stable across retries.
// Overflow here falls back to the drop-oldest rule so a prolonged outage
// can't OOM the slave.
func (p *PushSink) Requeue(payloads []cluster.CyclePayload) {
	if len(payloads) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(payloads) - 1; i >= 0; i-- {
		if p.size == p.cap {
			// Drop tail (newest) to make room — requeued data is older and we
			// want to preserve ordering with new cycles that arrived mid-retry.
			p.size--
			p.dropped++
		}
		p.head = (p.head - 1 + p.cap) % p.cap
		p.buf[p.head] = payloads[i]
		p.size++
	}
}

// Len reports the current buffered count. Diagnostic only: the push loop drains
// batchLimit once per tick and never consults it.
func (p *PushSink) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.size
}
