package probe

import (
	"slices"
	"sync"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
)

// health remembers the last outcome per backend so the runner logs transitions
// rather than results, and keeps the message behind a current failure.
type health struct {
	mu    sync.Mutex
	units map[unitKey]*unitState
	// orphans are scopes a unit wrote under before its labels changed.
	orphans []metrics.Labels
}

// unitKey names one thing a run reports on: a backend, or resolveUnit for the
// target's resolution.
type unitKey struct{ target, backend string }

const resolveUnit = "resolve"

func (k unitKey) String() string { return k.target + "|" + k.backend }

type unitState struct {
	up   bool
	why  Failure
	seen bool
	// scope is the label set the unit's series are written under, kept so they can
	// be retired once the unit is gone.
	scope metrics.Labels
}

func newHealth() *health { return &health{units: map[unitKey]*unitState{}} }

func (h *health) begin() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, u := range h.units {
		u.seen = false
	}
}

// changed records the outcome and reports whether it differs from the last one.
// The first result for a key counts as a change only when it is a failure.
func (h *health) changed(key unitKey, scope metrics.Labels, ok bool, err error) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	u, known := h.units[key]
	if !known {
		u = &unitState{}
		h.units[key] = u
	}
	// Same unit, new labels: nobody is left to write the old ones.
	if known && !slices.Equal(u.scope, scope) {
		h.orphans = append(h.orphans, u.scope)
	}
	previous := u.up
	u.up, u.seen, u.scope, u.why = ok, true, scope, Failure{}
	if !ok && err != nil {
		u.why = Failure{Reason: ReasonOf(err), Message: Message(err)}
	}
	if !known {
		return !ok
	}
	return previous != ok
}

// prune forgets the units the run did not reach and returns their scopes.
func (h *health) prune() []metrics.Labels {
	h.mu.Lock()
	defer h.mu.Unlock()
	gone := h.orphans
	h.orphans = nil
	for key, u := range h.units {
		if !u.seen {
			gone = append(gone, u.scope)
			delete(h.units, key)
		}
	}
	return gone
}

func (h *health) down() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, u := range h.units {
		if !u.up {
			n++
		}
	}
	return n
}

// Failure is why a unit is failing: the category its metrics carry, and the
// message they cannot.
type Failure struct {
	Reason  FailureReason
	Message string
}

func (h *health) failures() map[string]Failure {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out map[string]Failure
	for key, u := range h.units {
		if u.up || u.why == (Failure{}) {
			continue
		}
		if out == nil {
			out = map[string]Failure{}
		}
		out[key.String()] = u.why
	}
	return out
}
