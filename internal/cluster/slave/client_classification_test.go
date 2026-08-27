package slave

import "testing"

// The master's own refusal classification has to land on the right side of the
// slave's drop/retry split, which is what makes these two facts one contract.
func TestMasterDecodeStatusesLandOnTheIntendedSide(t *testing.T) {
	// A body that stopped arriving must requeue, not drop.
	if !retryable4xx(408) {
		t.Error("408 is not retryable, so a read deadline on a slow push drops the batch")
	}
	// A body past the ingest cap can never fit; dropping is correct, and it must
	// not be fatal the way the permanent marker is.
	if retryable4xx(413) {
		t.Error("413 is retryable, so an oversize batch head-of-line blocks the ring forever")
	}
}
