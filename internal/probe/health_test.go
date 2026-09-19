package probe

import (
	"errors"
	"testing"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
)

func TestHealthReportsOnlyChanges(t *testing.T) {
	h := newHealth()

	if !h.changed(unitKey{"site", "a"}, nil, false, errors.New("boom")) {
		t.Error("the first failure must be reported")
	}
	if h.changed(unitKey{"site", "a"}, nil, false, errors.New("boom")) {
		t.Error("a repeated failure was reported again")
	}
	if !h.changed(unitKey{"site", "a"}, nil, true, nil) {
		t.Error("the recovery was not reported")
	}
	if h.changed(unitKey{"site", "a"}, nil, true, nil) {
		t.Error("a repeated success was reported")
	}
	if h.changed(unitKey{"site", "b"}, nil, true, nil) {
		t.Error("the first success was reported")
	}
}

func TestHealthCountsDownBackends(t *testing.T) {
	h := newHealth()
	h.changed(unitKey{"site", "a"}, nil, false, errors.New("boom"))
	h.changed(unitKey{"site", "b"}, nil, false, errors.New("boom"))
	h.changed(unitKey{"site", "c"}, nil, true, nil)
	if got := h.down(); got != 2 {
		t.Fatalf("down = %d, want 2", got)
	}
}

func TestHealthPrunesUnseenKeys(t *testing.T) {
	h := newHealth()
	h.changed(unitKey{"site", "a"}, nil, false, errors.New("boom"))
	h.changed(unitKey{"site", "b"}, metrics.L("backend", "b"), false, errors.New("boom"))
	h.prune()

	h.begin()
	h.changed(unitKey{"site", "a"}, nil, false, errors.New("boom"))
	// The scope comes back so the series written under it can be retired.
	if gone := h.prune(); len(gone) != 1 || gone[0].Get("backend") != "b" {
		t.Fatalf("pruned scopes = %v, want b's", gone)
	}

	if got := h.down(); got != 1 {
		t.Fatalf("down = %d after pruning, want 1", got)
	}
	if !h.changed(unitKey{"site", "b"}, nil, false, errors.New("boom")) {
		t.Error("a forgotten backend did not report its failure")
	}
}

func TestHealthKeepsTheMessageBehindAFailure(t *testing.T) {
	h := newHealth()
	h.changed(unitKey{"site", "a"}, nil, false, errors.New("connection refused"))
	h.changed(unitKey{"site", "b"}, nil, true, nil)

	if got := h.failures(); got["site|a"].Message != "connection refused" {
		t.Fatalf("failures = %v, want the message for a", got)
	}
	if _, ok := h.failures()["site|b"]; ok {
		t.Error("a healthy backend carries a message")
	}

	h.changed(unitKey{"site", "a"}, nil, true, nil)
	if got := h.failures(); len(got) != 0 {
		t.Fatalf("the message survived recovery: %v", got)
	}
}

func TestHealthPrunesMessages(t *testing.T) {
	h := newHealth()
	h.changed(unitKey{"site", "a"}, nil, false, errors.New("boom"))
	h.prune()
	h.begin()
	h.prune()
	if got := h.failures(); len(got) != 0 {
		t.Fatalf("messages survived pruning: %v", got)
	}
}
