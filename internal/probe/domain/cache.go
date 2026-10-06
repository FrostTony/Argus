package domain

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

const (
	defaultRefresh = 6 * time.Hour
	// retryAfter is how long a failure, or a registration near its end, is believed.
	retryAfter = 30 * time.Minute
	// keepStale is how long a good answer stands in for a registry that stopped answering.
	keepStale = 24 * time.Hour
)

// answers is shared by every domain probe, so that targets and probes naming one
// domain, and the generations a reload replaces, ask its registry once.
var answers = newCache()

// cache keeps registry answers by source and name, one lookup in flight per key.
type cache struct {
	mu sync.Mutex
	m  map[string]*entry
}

// entry is immutable once done is closed.
type entry struct {
	done    chan struct{}
	rec     *record
	err     error
	fetched time.Time // when rec came from the registry
	expires time.Time
}

func newCache() *cache { return &cache{m: map[string]*entry{}} }

// get returns the answer for key while it is fresh and fetches it otherwise; fetched
// reports a real lookup. ttl says how long a record is believed, ttl(nil) a failure.
func (c *cache) get(ctx context.Context, key string,
	fetch func(context.Context) (*record, error), ttl func(*record) time.Duration,
) (a *entry, fetched bool) {
	for {
		c.mu.Lock()
		prev := c.m[key]
		if prev != nil {
			select {
			case <-prev.done:
				if time.Now().Before(prev.expires) {
					c.mu.Unlock()
					return prev, false
				}
			default:
				c.mu.Unlock()
				select {
				case <-prev.done:
					continue
				case <-ctx.Done():
					return &entry{err: probe.Wrap(probe.ReasonConnect, ctx.Err())}, false
				}
			}
		}
		next := &entry{done: make(chan struct{})}
		c.m[key] = next
		c.sweepLocked()
		c.mu.Unlock()

		rec, err := fetch(ctx)

		c.mu.Lock()
		c.settleLocked(key, prev, next, rec, err, ttl)
		c.mu.Unlock()
		close(next.done)
		return next, true
	}
}

func (c *cache) settleLocked(key string, prev, next *entry, rec *record, err error, ttl func(*record) time.Duration) {
	now := time.Now()
	next.expires = now.Add(ttl(nil))
	switch {
	case err == nil:
		next.rec, next.fetched = rec, now
		next.expires = now.Add(ttl(rec))
	case errors.Is(err, context.Canceled):
		// A reload's cancellation says nothing about the name: waiters fetch again.
		next.err, next.expires = err, now
		if prev != nil {
			c.m[key] = prev
		} else {
			delete(c.m, key)
		}
	case prev != nil && prev.rec != nil && now.Sub(prev.fetched) < keepStale &&
		probe.ReasonOf(err) != probe.ReasonContent:
		// A registry that is down or rate-limiting has not changed the registration.
		next.rec, next.fetched = prev.rec, prev.fetched
	default:
		next.err = err
	}
}

// sweepLocked forgets names no probe has asked about since their answer went stale.
func (c *cache) sweepLocked() {
	cutoff := time.Now().Add(-keepStale)
	for key, a := range c.m {
		select {
		case <-a.done:
			if a.expires.Before(cutoff) {
				delete(c.m, key)
			}
		default:
		}
	}
}
