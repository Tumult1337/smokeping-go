package master

import (
	"sync"

	"github.com/tumult/gosmokeping/internal/cluster"
)

// cycleID is the tuple that identifies one measurement: the resolved
// group/name key and the cycle's own timestamp. Source is the map level above
// it, so two slaves probing one host never collide.
type cycleID struct {
	target string
	nano   int64
}

// dedupWindowPerSource is how many recent cycle identities one source's window
// holds. PushSink.Requeue puts a failed batch back at the ring's *head*, so the
// next Drain re-sends exactly those cycles before any newer one: the whole
// redelivered set is one batch, and cluster.MaxCyclesPerBatch is the largest
// batch this master will accept. Sizing the window below that would refuse to
// recognise a redelivery a conformant slave is entitled to send.
const dedupWindowPerSource = cluster.MaxCyclesPerBatch

// dedupMaxSources is maxRegisteredSlaves because ingest refuses a name the
// registry does not hold, so the registry's own ceiling is the number of
// windows that can be live at once.
const dedupMaxSources = maxRegisteredSlaves

// sourceWindow is a fixed-capacity insertion-ordered set of cycle identities.
// It is a window rather than a high-water mark on purpose: a backlog delivered
// after an outage arrives with timestamps older than cycles already stored, and
// every one of those is a real measurement that must still be written.
type sourceWindow struct {
	seen map[cycleID]struct{}
	// names interns target keys so one target's entries share one backing
	// array rather than one per cycle, which is what keeps the window's memory
	// scaling with distinct targets instead of with cycle rate. A window of N
	// entries references at most N distinct keys, so dedupWindowPerSource is
	// the table's own ceiling and outgrowing it means the entries that put
	// those keys there are gone.
	names map[string]string
	ring  []cycleID
	next  int
	// used is the dedup's monotonic counter at this window's last admit call,
	// which is what picks the victim when a new source needs a slot.
	used uint64
}

func (w *sourceWindow) admit(target string, nano int64) bool {
	if len(w.names) > dedupWindowPerSource {
		clear(w.names)
	}
	canonical, ok := w.names[target]
	if !ok {
		canonical = target
		w.names[target] = target
	}
	id := cycleID{target: canonical, nano: nano}
	if _, dup := w.seen[id]; dup {
		return false
	}
	if len(w.ring) < dedupWindowPerSource {
		w.ring = append(w.ring, id)
	} else {
		delete(w.seen, w.ring[w.next])
		w.ring[w.next] = id
		w.next = (w.next + 1) % dedupWindowPerSource
	}
	w.seen[id] = struct{}{}
	return true
}

// cycleDedup admits each measurement into the fanout once. It sits at the
// ingest boundary, upstream of the fanout, so the storage writer and the alert
// evaluator are both covered by one guard; what a sink does with a cycle it
// was handed is past the guarantee, since OnCycle reports nothing back.
type cycleDedup struct {
	mu       sync.Mutex
	clock    uint64
	bySource map[string]*sourceWindow
	// registered reports whether a source is still one the registry holds, so
	// eviction spends a window whose source is gone before one that is merely
	// between pushes. Nil in unit tests, where no registry is coupled and
	// eviction is plain LRU.
	registered func(string) bool
}

func newCycleDedup() *cycleDedup {
	return &cycleDedup{bySource: make(map[string]*sourceWindow)}
}

// admit reports whether this (source, target, timestamp) has not been ingested
// within the source's window, recording it when it has not.
func (d *cycleDedup) admit(source, target string, nano int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.clock++
	w, ok := d.bySource[source]
	if !ok {
		if len(d.bySource) >= dedupMaxSources {
			d.evictLRU()
		}
		w = &sourceWindow{seen: make(map[cycleID]struct{}), names: make(map[string]string)}
		d.bySource[source] = w
	}
	w.used = d.clock
	return w.admit(target, nano)
}

// forget releases an identity this window reserved for a delivery that never
// completed. The ring keeps the slot as a tombstone: eviction deletes a key
// that is already gone, which costs a no-op rather than a scan.
func (d *cycleDedup) forget(source, target string, nano int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	w, ok := d.bySource[source]
	if !ok {
		return
	}
	delete(w.seen, cycleID{target: target, nano: nano})
}

// forgetSource drops a window whose source the registry has released. Ingest
// refuses a name the registry does not hold, so nothing consults it again.
func (d *cycleDedup) forgetSource(source string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.bySource, source)
}

// evictLRU drops a window to make room, preferring one whose source the
// registry no longer holds and falling back to the least recently used —
// eviction never refuses the newcomer. Reached only past dedupMaxSources
// distinct names, which the registry's own ceiling makes transient; the
// evicted source degrades to the pre-guard behaviour rather than losing data.
// Must be called with d.mu held.
func (d *cycleDedup) evictLRU() {
	var victim string
	var oldest uint64
	var victimDead, found bool
	for name, w := range d.bySource {
		dead := d.registered != nil && !d.registered(name)
		switch {
		case !found, dead && !victimDead:
		case dead == victimDead && w.used < oldest:
		default:
			continue
		}
		victim, oldest, victimDead, found = name, w.used, dead, true
	}
	if found {
		delete(d.bySource, victim)
	}
}
