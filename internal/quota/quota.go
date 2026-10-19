// Package quota keeps count of the requests each probe has sent in its current
// period, so that a probe whose target charges per call or limits its callers
// stops at the number it was given. The count is saved to a file and read back
// at start: a restart, a reload or a crash loop must not hand out the period
// again.
//
// Nodes do not share a count. A probe on several nodes has a quota on each,
// which is what whoever renders the configuration divides.
package quota

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
)

// Ledger holds the count of every probe with a quota, by probe name. Usage
// stays with the name across reloads, and a probe taken out of the
// configuration and put back within its period finds it where it was.
type Ledger struct {
	path string
	log  *slog.Logger

	mu       sync.Mutex
	accounts map[string]*entry
	// noted is, per probe, the end of the period whose exhaustion has been
	// reported, so a probe refused every minute — or a dashboard polling
	// /probe — says so once.
	noted map[string]time.Time
	// failing is set while saving fails, so the log says so once per spell.
	failing bool
}

// entry is one probe's count in one period.
type entry struct {
	Per   string    `json:"per"`
	Start time.Time `json:"start"`
	Used  int64     `json:"used"`
}

type file struct {
	Version  int               `json:"version"`
	Accounts map[string]*entry `json:"accounts"`
}

// Open reads the ledger saved at path. No path keeps the counts in memory; a
// missing file is an empty ledger, and an unreadable one is reported and
// started over rather than keeping the node down.
func Open(path string, log *slog.Logger) *Ledger {
	l := &Ledger{path: path, log: log, accounts: map[string]*entry{}, noted: map[string]time.Time{}}
	if path == "" {
		return l
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return l
	case err != nil:
		log.Error("cannot read the quota ledger, counting from zero", "path", path, "err", err)
		return l
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		log.Error("the quota ledger is unreadable, counting from zero", "path", path, "err", err)
		return l
	}
	for name, e := range f.Accounts {
		if e != nil {
			l.accounts[name] = e
		}
	}
	return l
}

// Account is the probe's handle on its count under the limit it now has.
func (l *Ledger) Account(name string, q *config.Quota, loc *time.Location) *Account {
	if l == nil || q == nil {
		return nil
	}
	return &Account{ledger: l, name: name, limit: *q, loc: loc}
}

// Account draws one probe's requests from the ledger.
type Account struct {
	ledger *Ledger
	name   string
	limit  config.Quota
	loc    *time.Location
}

// Usage is an account as it stands.
type Usage struct {
	Limit  int       `json:"limit"`
	Per    string    `json:"per"`
	Used   int64     `json:"used"`
	Resets time.Time `json:"resets"`
}

// Remaining is what is left of the period's quota; never negative.
func (u Usage) Remaining() int64 { return max(0, int64(u.Limit)-u.Used) }

// Ratio is how much of the period's quota is gone: 0 untouched, 1 spent. It
// can pass 1, as a run is charged what it sent: the hops of a redirect chain
// are known only once followed.
func (u Usage) Ratio() float64 {
	if u.Limit <= 0 {
		return 0
	}
	return float64(u.Used) / float64(u.Limit)
}

// current is the probe's entry for the period holding now, a new period
// starting from zero. Called with mu held.
func (a *Account) current(now time.Time) (*entry, time.Time) {
	start, end := a.limit.Window(now, a.loc)
	e := a.ledger.accounts[a.name]
	if e == nil || e.Per != a.limit.Per || !e.Start.Equal(start) {
		e = &entry{Per: a.limit.Per, Start: start}
		a.ledger.accounts[a.name] = e
	}
	return e, end
}

// Usage reports the count in the period holding now.
func (a *Account) Usage(now time.Time) Usage {
	if a == nil {
		return Usage{}
	}
	a.ledger.mu.Lock()
	defer a.ledger.mu.Unlock()
	e, end := a.current(now)
	return Usage{Limit: a.limit.Requests, Per: a.limit.Per, Used: e.Used, Resets: end}
}

// Reserve takes n requests ahead of a run, all or nothing: a run that would
// not fit whole does not start, since half a run reports half the targets
// down. The answer is the usage after the decision either way.
func (a *Account) Reserve(n int, now time.Time) (bool, Usage) {
	if a == nil {
		return true, Usage{}
	}
	a.ledger.mu.Lock()
	defer a.ledger.mu.Unlock()
	e, end := a.current(now)
	u := Usage{Limit: a.limit.Requests, Per: a.limit.Per, Used: e.Used, Resets: end}
	if n <= 0 {
		return true, u
	}
	if e.Used+int64(n) > int64(a.limit.Requests) {
		return false, u
	}
	e.Used += int64(n)
	u.Used = e.Used
	a.ledger.saveLocked()
	return true, u
}

// FirstRefusal reports whether a refusal in the period ending at resets is the
// first one for this probe, scheduled and on-demand runs alike.
func (a *Account) FirstRefusal(resets time.Time) bool {
	if a == nil {
		return false
	}
	a.ledger.mu.Lock()
	defer a.ledger.mu.Unlock()
	if a.ledger.noted[a.name].Equal(resets) {
		return false
	}
	a.ledger.noted[a.name] = resets
	return true
}

// Settle replaces a reservation with what the run sent. It goes into the
// period the reservation was made in; a run that crossed into the next one is
// charged where it started, and the new period is left whole.
func (a *Account) Settle(reserved, sent int, at time.Time) {
	if a == nil || reserved == sent {
		return
	}
	a.ledger.mu.Lock()
	defer a.ledger.mu.Unlock()
	start, _ := a.limit.Window(at, a.loc)
	e := a.ledger.accounts[a.name]
	if e == nil || e.Per != a.limit.Per || !e.Start.Equal(start) {
		return // the period has turned; the old one is closed
	}
	e.Used = max(0, e.Used+int64(sent-reserved))
	a.ledger.saveLocked()
}

// saveLocked writes the ledger whole, through a file renamed into place so
// that a crash leaves the old count or the new one and never half of either.
// A failure is logged, once until it succeeds again, and the count stays in
// memory: losing it at the next restart is better than refusing to probe.
func (l *Ledger) saveLocked() {
	if l.path == "" {
		return
	}
	err := l.write()
	switch {
	case err != nil && !l.failing:
		l.failing = true
		l.log.Error("cannot save the quota ledger; counts survive only until a restart", "path", l.path, "err", err)
	case err == nil && l.failing:
		l.failing = false
		l.log.Info("the quota ledger is saved again", "path", l.path)
	}
}

func (l *Ledger) write() error {
	data, err := json.MarshalIndent(file{Version: 1, Accounts: l.accounts}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(l.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".quota-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), l.path); err != nil {
		return fmt.Errorf("replace %s: %w", l.path, err)
	}
	return nil
}
