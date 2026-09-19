package resolve

import (
	"context"
	"sync"
	"time"
)

// Cache wraps a resolver, holding each answer for its TTL clamped by min/max.
type Cache struct {
	Inner Resolver
	// TTL, when set, overrides the TTL from the answer.
	TTL    time.Duration
	MinTTL time.Duration
	MaxTTL time.Duration

	// MaxEntries caps the cache; names come from a churning discovery source.
	MaxEntries int

	mu      sync.Mutex
	entries map[string]*entry
	now     func() time.Time
}

type entry struct {
	res     Result
	expires time.Time
	// inflight collapses concurrent lookups of the same name into one.
	inflight chan struct{}
	err      error
	// abandoned marks a lookup whose caller gave up; waiters ask again.
	abandoned bool
}

const defaultMaxEntries = 10_000

func NewCache(inner Resolver, ttl, min, max time.Duration) *Cache {
	return &Cache{
		Inner:      inner,
		TTL:        ttl,
		MinTTL:     min,
		MaxTTL:     max,
		MaxEntries: defaultMaxEntries,
		entries:    make(map[string]*entry),
		now:        time.Now,
	}
}

// evictLocked drops expired entries, then entries nobody is waiting on.
func (c *Cache) evictLocked() {
	if c.MaxEntries <= 0 || len(c.entries) < c.MaxEntries {
		return
	}
	now := c.now()
	for k, e := range c.entries {
		if e.inflight == nil && now.After(e.expires) {
			delete(c.entries, k)
		}
	}
	for k, e := range c.entries {
		if len(c.entries) < c.MaxEntries {
			return
		}
		if e.inflight == nil {
			delete(c.entries, k)
		}
	}
}

func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func (c *Cache) Resolve(ctx context.Context, host string, family Family) (Result, error) {
	key := host + "|" + string(family)
	for {
		res, settled, err := c.lookup(ctx, key, host, family)
		if settled {
			return res, err
		}
	}
}

// lookup is one pass over the cache. It reports false when the lookup it waited
// for was abandoned and the question has to be asked again.
func (c *Cache) lookup(ctx context.Context, key, host string, family Family) (res Result, settled bool, err error) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok {
		if e.inflight != nil {
			// Take the channel under the lock: the leader clears the field,
			// and a waiter reading it afterwards would receive from nil.
			wait := e.inflight
			c.mu.Unlock()
			select {
			case <-wait:
			case <-ctx.Done():
				return Result{}, true, ctx.Err()
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			return cachedCopy(e.res), !e.abandoned, e.err
		}
		if c.now().Before(e.expires) {
			// A cached failure comes back as a failure, never as an empty success.
			defer c.mu.Unlock()
			return cachedCopy(e.res), true, e.err
		}
	}
	c.evictLocked()
	e := &entry{inflight: make(chan struct{})}
	c.entries[key] = e
	c.mu.Unlock()

	res, err = c.Inner.Resolve(ctx, host, family)

	c.mu.Lock()
	e.res, e.err = res, err
	e.expires = c.now().Add(c.lifetime(res.TTL, err))
	// The caller giving up says nothing about the name, so nothing is cached.
	if err != nil && ctx.Err() != nil {
		e.abandoned = true
		delete(c.entries, key)
	}
	close(e.inflight)
	e.inflight = nil
	c.mu.Unlock()
	return res, true, err
}

// lifetime caches a failed lookup for MinTTL too, so a broken zone is not
// queried on every probe.
func (c *Cache) lifetime(ttl time.Duration, err error) time.Duration {
	if err != nil {
		return max(c.MinTTL, time.Second)
	}
	if c.TTL > 0 {
		return c.TTL
	}
	if ttl <= 0 {
		ttl = c.MinTTL
	}
	if c.MinTTL > 0 && ttl < c.MinTTL {
		ttl = c.MinTTL
	}
	if c.MaxTTL > 0 && ttl > c.MaxTTL {
		ttl = c.MaxTTL
	}
	return max(ttl, time.Second)
}

// cachedCopy marks the answer as served from memory, keeping Duration.
func cachedCopy(r Result) Result {
	r.Cached = true
	return r
}
