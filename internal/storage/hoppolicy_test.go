package storage

import (
	"sync"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/probe"
	"github.com/tumult/gosmokeping/internal/scheduler"
)

func cycleWithLastHopLoss(loss int) scheduler.Cycle {
	return scheduler.Cycle{
		Time:   time.Unix(1_700_000_000, 0),
		Target: config.TargetRef{Group: "g", Target: config.Target{Name: "t"}},
		Hops: []probe.Hop{
			{Index: 0, Sent: 10, Lost: 0},
			{Index: 1, Sent: 10, Lost: loss},
		},
	}
}

func TestHopPolicy_AlwaysWrites(t *testing.T) {
	p, err := NewHopPolicy("always", 0)
	if err != nil {
		t.Fatalf("NewHopPolicy: %v", err)
	}
	if !p.ShouldWrite(cycleWithLastHopLoss(0)) {
		t.Fatal("always mode must write when no loss")
	}
	if !p.ShouldWrite(cycleWithLastHopLoss(3)) {
		t.Fatal("always mode must write when loss")
	}
}

func TestHopPolicy_OnLossSkipsWhenClean(t *testing.T) {
	p, err := NewHopPolicy("on_loss", 0)
	if err != nil {
		t.Fatalf("NewHopPolicy: %v", err)
	}
	if p.ShouldWrite(cycleWithLastHopLoss(0)) {
		t.Fatal("on_loss must skip clean cycle")
	}
	if !p.ShouldWrite(cycleWithLastHopLoss(1)) {
		t.Fatal("on_loss must write on any loss")
	}
}

func TestHopPolicy_OnLossSkipsCyclesWithNoHops(t *testing.T) {
	p, _ := NewHopPolicy("on_loss", 0)
	c := scheduler.Cycle{Time: time.Unix(1_700_000_000, 0)}
	if p.ShouldWrite(c) {
		t.Fatal("ShouldWrite must be false when c.Hops is empty")
	}
}

func TestHopPolicy_SampledWritesOncePerBucketPerTarget(t *testing.T) {
	p, err := NewHopPolicy("sampled", 30*time.Minute)
	if err != nil {
		t.Fatalf("NewHopPolicy: %v", err)
	}
	c := cycleWithLastHopLoss(0)
	// First call in a bucket: baseline write.
	if !p.ShouldWrite(c) {
		t.Fatal("first cycle in bucket must be sampled")
	}
	// Same target, same bucket, still clean: skip.
	c.Time = c.Time.Add(time.Minute)
	if p.ShouldWrite(c) {
		t.Fatal("second clean cycle in same bucket must skip")
	}
	// Advance past bucket boundary: write again.
	c.Time = c.Time.Add(30 * time.Minute)
	if !p.ShouldWrite(c) {
		t.Fatal("cycle in next bucket must be sampled")
	}
}

func TestHopPolicy_SampledWritesOnLossEvenInsideBucket(t *testing.T) {
	p, _ := NewHopPolicy("sampled", 30*time.Minute)
	base := cycleWithLastHopLoss(0)
	if !p.ShouldWrite(base) {
		t.Fatal("baseline write must succeed")
	}
	// Loss cycle two minutes later, same bucket: must still write.
	lossy := cycleWithLastHopLoss(2)
	lossy.Time = base.Time.Add(2 * time.Minute)
	if !p.ShouldWrite(lossy) {
		t.Fatal("sampled must always write on loss inside the same bucket")
	}
	// Now that loss consumed the baseline slot, a subsequent clean cycle in
	// the same bucket must skip.
	clean := cycleWithLastHopLoss(0)
	clean.Time = base.Time.Add(3 * time.Minute)
	if p.ShouldWrite(clean) {
		t.Fatal("clean cycle in same bucket after loss must skip (loss consumed baseline)")
	}
}

func TestHopPolicy_RejectsUnknownMode(t *testing.T) {
	if _, err := NewHopPolicy("nope", 0); err == nil {
		t.Fatal("expected error for unknown mode")
	}
}

func TestHopPolicy_SampledRequiresPositiveEvery(t *testing.T) {
	if _, err := NewHopPolicy("sampled", 0); err == nil {
		t.Fatal("sampled mode must require positive sample_every")
	}
}

func TestHopPolicy_SampledKeyedByTargetAndSource(t *testing.T) {
	p, _ := NewHopPolicy("sampled", 30*time.Minute)
	c1 := cycleWithLastHopLoss(0)
	c1.Source = "slave-a"
	c2 := cycleWithLastHopLoss(0)
	c2.Source = "slave-b"
	if !p.ShouldWrite(c1) {
		t.Fatal("slave-a first cycle must be sampled")
	}
	if !p.ShouldWrite(c2) {
		t.Fatal("slave-b first cycle must be sampled independently of slave-a")
	}
	// Second cycle on same source same bucket: skip.
	if p.ShouldWrite(c1) {
		t.Fatal("slave-a second cycle in same bucket must skip")
	}
}

func TestHopPolicy_SampledConcurrentAccess(t *testing.T) {
	p, _ := NewHopPolicy("sampled", time.Minute)
	c := cycleWithLastHopLoss(0)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			p.ShouldWrite(c)
		})
	}
	wg.Wait()
}

func TestHopPolicy_SampledAcceptsSubSecondSampleEvery(t *testing.T) {
	// Sub-second sampleEvery must not panic. With the nanosecond bucket math
	// any positive duration the constructor allows is integer-safe.
	p, err := NewHopPolicy("sampled", 500*time.Millisecond)
	if err != nil {
		t.Fatalf("NewHopPolicy: %v", err)
	}
	c := cycleWithLastHopLoss(0)
	if !p.ShouldWrite(c) {
		t.Fatal("first cycle must write")
	}
	// Same instant, same key: skip.
	if p.ShouldWrite(c) {
		t.Fatal("same-bucket repeat must skip")
	}
	// Advance past the 500ms bucket boundary: write again.
	c.Time = c.Time.Add(time.Second)
	if !p.ShouldWrite(c) {
		t.Fatal("cycle in next bucket must write")
	}
}
