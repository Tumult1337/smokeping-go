package slave

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/scheduler"
	"github.com/tumult/gosmokeping/internal/slavehealth"
)

// Bytes one buffered entry costs outside the payload's own heap. Taken with
// unsafe.Sizeof rather than written as literals so a field added to either DTO
// moves the accounting on its own. Nothing tests that it does: an assertion
// against unsafe.Sizeof would be comparing the same expression to itself, and
// the only edit that could redden it is the one that adds the field.
const (
	payloadStructBytes = int64(unsafe.Sizeof(cluster.CyclePayload{}))
	hopStructBytes     = int64(unsafe.Sizeof(cluster.HopDTO{}))
	httpStructBytes    = int64(unsafe.Sizeof(cluster.HTTPSampleDTO{}))
	durationBytes      = int64(unsafe.Sizeof(int64(0)))
)

// initialRingEntries is where the ring starts. It grows on demand, so a large
// budget costs a large array only once the cycles to fill it exist.
const initialRingEntries = 64

// PushSink is the only scheduler.Sink the slave's scheduler runs against. It
// buffers every cycle in a ring the runner drains and ships on its push
// cadence.
//
// The ring is bounded in bytes rather than in entries, because a buffered
// cycle's size spans two orders of magnitude: 557 B with no hops, 2,671 B for
// a typical icmp trace, 52,157 B for an mtr walk at its producer ceiling. An
// entry count sized for one of those is wrong for the others.
// TestDocumentedCycleSizesAreTheOnesTheAccountingProduces computes all three.
//
// Past the budget the sink sheds in two steps, in this order:
//
//  1. strip Hops from the oldest cycle still carrying them, and
//  2. only once no buffered cycle carries hops, drop the oldest cycle whole.
//
// Loss, RTTs and the percentile summary therefore survive an outage that the
// path history does not — the graph an operator opens during one, and every
// alert condition, read the first set.
type PushSink struct {
	log *slog.Logger
	mu  sync.Mutex
	// buf is a ring buffer. head/size index into it; it grows while the
	// budget allows and never shrinks.
	buf  []cluster.CyclePayload
	head int
	size int

	// budget bounds cap(buf)*payloadStructBytes + heap. The backing array is
	// inside it, which is what makes it a ceiling on accounted live bytes
	// rather than an estimate of one — see config.DefaultBufferBytes for the
	// three gaps between that and resident memory.
	budget int64
	heap   int64

	// stripIdx is a logical offset from head, and the invariant it carries is
	// load-bearing: every live entry below it has already been shed. A stale
	// value cannot corrupt the ring — the walk skips a hopless entry and
	// advances past it — but one that sits above a live hop-bearing entry
	// makes stripOldestHops report nothing to shed, and makeRoom then drops a
	// measurement it could have kept. Every site that moves head or size
	// maintains it.
	stripIdx int

	dropped  int
	stripped int
	// lastStripLog and lastDropLog are the counter values reportLocked last
	// emitted a line for.
	lastStripLog int
	lastDropLog  int

	// hopMarkers mirrors the master's advertisement, read on every cycle
	// rather than captured, so a master downgrade takes effect at the next
	// config pull. Zero value false is the fail-closed state a slave holds
	// before its first pull.
	hopMarkers atomic.Bool
}

// NewPushSink constructs a ring-buffered sink bounded at budget bytes. A
// non-positive budget falls back to config.DefaultBufferBytes.
func NewPushSink(log *slog.Logger, budget int64) *PushSink {
	if budget <= 0 {
		budget = config.DefaultBufferBytes
	}
	return &PushSink{
		log:    log,
		buf:    make([]cluster.CyclePayload, initialRingEntries),
		budget: budget,
	}
}

// SetHopMarkers records whether the master understands the TargetReply hop
// marker, from its /config advertisement.
func (p *PushSink) SetHopMarkers(ok bool) { p.hopMarkers.Store(ok) }

// hopBytes reports the heap a hop slice retains.
func hopBytes(hops []cluster.HopDTO) int64 {
	n := int64(cap(hops)) * hopStructBytes
	for _, h := range hops {
		n += int64(len(h.IP)) + int64(len(h.Unreach)) + int64(cap(h.RTTs))*durationBytes
	}
	return n
}

// payloadHeapBytes reports the heap a buffered payload retains outside the
// CyclePayload struct itself, which the ring's own array already accounts for.
// stats.Summary is time.Duration fields only and retains nothing.
func payloadHeapBytes(p cluster.CyclePayload) int64 {
	n := int64(len(p.Group)) + int64(len(p.Name)) + int64(len(p.ProbeName)) + int64(len(p.Source))
	n += int64(cap(p.RTTs)) * durationBytes
	n += hopBytes(p.Hops)
	n += int64(cap(p.HTTPSamples)) * httpStructBytes
	for _, s := range p.HTTPSamples {
		n += int64(len(s.Err))
	}
	return n
}

// at returns a pointer to the entry at logical offset i from head.
func (p *PushSink) at(i int) *cluster.CyclePayload {
	return &p.buf[(p.head+i)%len(p.buf)]
}

// entriesFor is the ring size that holds size+n entries, doubling from have.
// The single place the doubling is written: fits and growFor consulted separate
// copies of it, which is a pair of expressions that can drift apart.
func entriesFor(have, size, n int) int {
	for size+n > have {
		have *= 2
	}
	return have
}

// fits reports whether n more payloads totalling heap bytes can be admitted
// inside the budget, counting every ring doubling they would need. It is the
// single growth gate: admission, requeue and the reclaim loop all consult this
// one predicate, so none can be relaxed while another still holds.
func (p *PushSink) fits(heap int64, n int) bool {
	return int64(entriesFor(len(p.buf), p.size, n))*payloadStructBytes+p.heap+heap <= p.budget
}

// stripOldestHops sheds the hops of the oldest entry still carrying any,
// advancing stripIdx past every hopless entry it walks over. Reports whether
// it stripped one.
func (p *PushSink) stripOldestHops() bool {
	for ; p.stripIdx < p.size; p.stripIdx++ {
		e := p.at(p.stripIdx)
		if len(e.Hops) == 0 {
			continue
		}
		p.heap -= hopBytes(e.Hops)
		e.Hops = nil
		p.stripIdx++
		p.stripped++
		return true
	}
	return false
}

// dropOldest removes the entry at head.
func (p *PushSink) dropOldest() {
	p.heap -= payloadHeapBytes(p.buf[p.head])
	p.buf[p.head] = cluster.CyclePayload{}
	p.head = (p.head + 1) % len(p.buf)
	p.size--
	if p.stripIdx > 0 {
		p.stripIdx--
	}
	p.dropped++
}

// makeRoom sheds until n payloads totalling heap bytes fit, or until the ring
// is empty. Emptying it is the exit: a payload alone in an empty sink is
// admitted past the budget, because refusing one that can never fit would
// refuse it again on every cycle for the life of the process.
// alert.dispatchShardBytes takes the same escape.
//
// evict removes one entry: the oldest on the admission path, the newest on the
// requeue path, where the batch going back is the older data.
//
// The loop terminates because each pass either strips one entry, of which
// there are at most size, or removes one.
func (p *PushSink) makeRoom(heap int64, n int, evict func()) {
	for p.size > 0 && !p.fits(heap, n) {
		if p.stripOldestHops() {
			continue
		}
		evict()
	}
}

// resize reallocates the ring at entries and re-linearises it so head is 0.
// Logical offsets are preserved, so stripIdx survives unchanged.
func (p *PushSink) resize(entries int) {
	next := make([]cluster.CyclePayload, entries)
	for i := 0; i < p.size; i++ {
		next[i] = *p.at(i)
	}
	p.buf = next
	p.head = 0
}

// growFor doubles the ring until n more entries fit.
func (p *PushSink) growFor(n int) {
	if entries := entriesFor(len(p.buf), p.size, n); entries != len(p.buf) {
		p.resize(entries)
	}
}

// shrinkIfIdle gives ring capacity back once the buffer has drained well below
// it. Without it the array only ever grows, and shedding is what drives that
// growth: a shed cycle is small, so more of them fit, so the ring doubles — and
// the array term is inside the budget, so one hopless backlog leaves most of
// the budget spent on empty slots for the life of the process and every later
// outage keeps less path history than the first. Measured at an 8 MiB budget, a
// second identical outage kept 69% of the hop rows the first did.
//
// The quarter-full trigger and the double-the-live-size target are hysteresis:
// a shrink must not put the ring straight back into growth.
func (p *PushSink) shrinkIfIdle() {
	if len(p.buf) <= initialRingEntries || p.size*4 >= len(p.buf) {
		return
	}
	entries := initialRingEntries
	for entries < p.size*2 {
		entries *= 2
	}
	if entries < len(p.buf) {
		p.resize(entries)
	}
}

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
	newHeap := payloadHeapBytes(payload)

	p.mu.Lock()
	p.makeRoom(newHeap, 1, p.dropOldest)
	p.growFor(1)
	*p.at(p.size) = payload
	p.size++
	p.heap += newHeap
	reports := p.reportLocked()
	p.mu.Unlock()
	p.emit(reports)
}

// shedLogEvery is how many shed or dropped cycles pass between warnings.
const shedLogEvery = 100

// shedReport is one warning the sink decided to emit, carried out from under
// the lock. slog writes synchronously and mu is the sink's only lock, held by
// every target's probe goroutine in turn — logging under it stalls OnCycle for
// every other target on the one path that runs when the buffer is already
// under pressure.
type shedReport struct {
	msg      string
	key      string
	count    int
	buffered int
	bytes    int64
	budget   int64
}

// reportLocked decides which warnings are due. It compares against the last
// count logged rather than testing count%shedLogEvery: makeRoom strips and
// drops in a loop, so the counters step over a multiple instead of landing on
// one, and an exact-modulus test skipped most of its own lines — measured, 9
// emitted against 29 crossings. Stripping and dropping are reported apart
// because they cost different things: a stripped cycle loses its path history,
// a dropped one loses the measurement.
func (p *PushSink) reportLocked() []shedReport {
	var out []shedReport
	if p.stripped-p.lastStripLog >= shedLogEvery {
		p.lastStripLog = p.stripped
		out = append(out, shedReport{
			msg: "push buffer over budget, shedding hop rows from the oldest cycles",
			key: "stripped", count: p.stripped, buffered: p.size, bytes: p.total(), budget: p.budget,
		})
	}
	if p.dropped-p.lastDropLog >= shedLogEvery {
		p.lastDropLog = p.dropped
		out = append(out, shedReport{
			msg: "push buffer full of hopless cycles, dropping the oldest",
			key: "dropped", count: p.dropped, buffered: p.size, bytes: p.total(), budget: p.budget,
		})
	}
	return out
}

func (p *PushSink) emit(reports []shedReport) {
	for _, r := range reports {
		p.log.Warn(r.msg, r.key, r.count, "buffered", r.buffered, "bytes", r.bytes, "budget", r.budget)
	}
}

// Drain returns up to max payloads and removes them from the buffer. Caller
// owns the slice. Returns nil/empty when the buffer is empty.
//
// Health-target hops are withheld here as well as at admission, because the
// advertisement that governs the decision is the one current when the batch
// goes on the wire, not when the cycle was buffered. A master restarted onto a
// binary predating the TargetReply marker clears hopMarkers at the next
// /config pull, and every health cycle already buffered still carries the
// slave's own address; a marker-blind master redacts by position and would
// serve it on the unauthenticated /hops. The buffer now holds tens of minutes
// rather than the ninety seconds it held before, and an outage is exactly when
// a master gets replaced. The admission-time clear stays: it is what keeps
// those bytes from being retained and accounted at all.
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
	markers := p.hopMarkers.Load()
	out := make([]cluster.CyclePayload, n)
	for i := 0; i < n; i++ {
		e := p.at(i)
		out[i] = *e
		if !markers && slavehealth.IsHealthGroup(out[i].Group) {
			out[i].Hops = nil
		}
		p.heap -= payloadHeapBytes(*e)
		*e = cluster.CyclePayload{}
	}
	p.head = (p.head + n) % len(p.buf)
	p.size -= n
	p.stripIdx -= n
	if p.stripIdx < 0 {
		p.stripIdx = 0
	}
	p.shrinkIfIdle()
	return out
}

// Requeue pushes a failed batch back onto the head of the ring. Used when a
// push errors with a retryable status; keeps ordering stable across retries.
// Overflow follows the same shed-then-drop ladder as OnCycle, with one
// inversion: it drops from the *tail*, because the requeued batch is older
// than the cycles that arrived during the retry and must keep its place ahead
// of them.
func (p *PushSink) Requeue(payloads []cluster.CyclePayload) {
	if len(payloads) == 0 {
		return
	}
	var heap int64
	for _, pl := range payloads {
		heap += payloadHeapBytes(pl)
	}

	p.mu.Lock()
	// Room is made for the whole batch in one pass and the prepend follows,
	// rather than the two interleaving. Every prepend shifts each live entry's
	// offset by one, so an interleaved reclaim leaves stripIdx pointing past
	// entries that still carry hops — the walk then skips them and the sink
	// drops a cycle it could have shed. Sizing once removes the shift from the
	// loop entirely.
	//
	// A batch that cannot fit an emptied ring is still taken whole, the same
	// escape OnCycle's lone payload gets. It is bounded by the runner's
	// batchLimit, roughly 6.6 MB of mtr cycles against the shipped 100.
	p.makeRoom(heap, len(payloads), p.dropNewest)
	p.growFor(len(payloads))
	for i := len(payloads) - 1; i >= 0; i-- {
		p.head = (p.head - 1 + len(p.buf)) % len(p.buf)
		p.buf[p.head] = payloads[i]
		p.size++
	}
	p.heap += heap
	// The prepended entries are unstripped and sit at the head, so the walk
	// starts there again. One re-walk per requeue, at most one per push tick:
	// the drain loop stops on the first failure.
	p.stripIdx = 0
	reports := p.reportLocked()
	p.mu.Unlock()
	p.emit(reports)
}

// dropNewest removes the entry at the tail. Requeue's counterpart to
// dropOldest: the batch being put back is the older data.
//
// It leaves stripIdx alone, which is safe only because Requeue is its sole
// caller and Requeue resets stripIdx to 0 before it returns. A second caller
// would have to clamp it: a stripIdx above size makes stripOldestHops report
// nothing to shed however many hop rows are buffered.
func (p *PushSink) dropNewest() {
	e := p.at(p.size - 1)
	p.heap -= payloadHeapBytes(*e)
	*e = cluster.CyclePayload{}
	p.size--
	p.dropped++
}

// total reports the accounted live bytes: the ring's own array plus every
// buffered payload's heap.
func (p *PushSink) total() int64 {
	return int64(len(p.buf))*payloadStructBytes + p.heap
}

// Len reports the current buffered count. Diagnostic only: the push loop
// decides from what flushOnce reports and never consults it.
func (p *PushSink) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.size
}

// Bytes reports the accounted live bytes the sink holds. Diagnostic only.
func (p *PushSink) Bytes() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.total()
}

// Budget reports the configured byte ceiling. Diagnostic only.
func (p *PushSink) Budget() int64 { return p.budget }
