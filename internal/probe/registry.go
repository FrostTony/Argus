package probe

import (
	"fmt"
	"sort"
	"sync"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
)

type Factory func(cfg config.Probe) (Prober, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register adds a prober type, from the prober package's init.
func Register(kind string, f Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[kind]; dup {
		panic("probe: type " + kind + " registered twice")
	}
	registry[kind] = f
}

func New(cfg config.Probe) (Prober, error) {
	registryMu.RLock()
	f, ok := registry[cfg.Type]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown probe type %q (available: %v)", cfg.Type, Kinds())
	}
	p, err := f(cfg)
	if err != nil {
		return nil, fmt.Errorf("probe %q (%s): %w", cfg.Name, cfg.Type, err)
	}
	return p, nil
}

func Kinds() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
