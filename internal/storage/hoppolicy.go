package storage

import (
	"fmt"
	"sync"
	"time"

	"github.com/tumult/gosmokeping/internal/scheduler"
)

// HopPolicy decides whether the per-cycle MTR/trace hop points get written to
// the raw bucket. probe_hop dominates raw-bucket size on busy installs (one
// point per hop per cycle) but is only operationally interesting when the
// target is actually losing packets; on_loss / sampled modes shed that bulk.
//
// Modes:
//   - "always":  every cycle that has hops writes them. Existing behaviour.
//   - "on_loss": writes only when the last hop in the trace reports Lost > 0.
//                Clean cycles drop the hop points entirely.
//   - "sampled": writes on loss (as on_loss) AND once per (target, source) per
//                time bucket of SampleEvery, so the path baseline is preserved
//                sparsely. Recommended when you still want "what does this
//                route normally look like" comparisons.
//
// ShouldWrite is the only public method. The caller must guard the
// `for _, hop := range c.Hops` loop with it.
type HopPolicy struct {
	mode        HopMode
	sampleEvery time.Duration

	mu         sync.Mutex
	lastBucket map[string]int64
}

type HopMode string

const (
	HopModeAlways  HopMode = "always"
	HopModeOnLoss  HopMode = "on_loss"
	HopModeSampled HopMode = "sampled"
)

// NewHopPolicy validates mode + sampleEvery and returns a ready policy.
// sampleEvery is ignored unless mode == "sampled"; it must be > 0 there.
func NewHopPolicy(mode string, sampleEvery time.Duration) (*HopPolicy, error) {
	m := HopMode(mode)
	if mode == "" {
		m = HopModeAlways
	}
	switch m {
	case HopModeAlways, HopModeOnLoss:
		return &HopPolicy{mode: m}, nil
	case HopModeSampled:
		if sampleEvery <= 0 {
			return nil, fmt.Errorf("hop_policy: sample_every must be > 0 for sampled mode")
		}
		return &HopPolicy{
			mode:        m,
			sampleEvery: sampleEvery,
			lastBucket:  make(map[string]int64),
		}, nil
	default:
		return nil, fmt.Errorf("hop_policy: unknown mode %q (want always|on_loss|sampled)", mode)
	}
}

// ShouldWrite reports whether the writer should emit probe_hop points for c.
// Empty Hops short-circuits to false so callers can skip the loop entirely.
// A nil receiver is treated as "always" so legacy callers that pre-date the
// policy keep working unchanged — Tasks 3/4 rely on this nil-safety.
func (p *HopPolicy) ShouldWrite(c scheduler.Cycle) bool {
	if p == nil || p.mode == HopModeAlways {
		return len(c.Hops) > 0
	}
	if len(c.Hops) == 0 {
		return false
	}
	last := c.Hops[len(c.Hops)-1]
	hasLoss := last.Lost > 0
	if p.mode == HopModeOnLoss {
		return hasLoss
	}
	// sampled: loss always wins; otherwise per-bucket baseline.
	if hasLoss {
		return true
	}
	key := c.Target.ID() + "|" + c.Source
	bucket := c.Time.Unix() / int64(p.sampleEvery.Seconds())
	p.mu.Lock()
	defer p.mu.Unlock()
	if prev, ok := p.lastBucket[key]; ok && prev == bucket {
		return false
	}
	p.lastBucket[key] = bucket
	return true
}
