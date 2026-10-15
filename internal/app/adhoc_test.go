package app

import (
	"context"
	"testing"
	"time"
)

// On-demand checks have slots of their own: with every scheduled slot taken,
// a check still runs, and the scheduled slots stay as they were.
func TestOnDemandChecksDoNotShareScheduledSlots(t *testing.T) {
	a, err := Build(testServer(), probesFrom(t, twoProbes), discard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	for range cap(a.sem) {
		a.sem <- struct{}{}
	}
	defer func() {
		for range cap(a.sem) {
			<-a.sem
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := a.Check(ctx, CheckRequest{Probe: "a"})
	if err != nil || !res.Success {
		t.Fatalf("check with the scheduled slots full: %+v, %v", res, err)
	}
	if len(a.adhoc) != 0 {
		t.Errorf("%d on-demand slots left held", len(a.adhoc))
	}
}
