package master

import (
	"sort"
	"sync"
	"time"
)

// SlaveInfo is one row in the in-memory registry. Kept deliberately small —
// anything that's expensive to track or that should outlive a master restart
// belongs in a persistent store, not here.
type SlaveInfo struct {
	Name     string    `json:"name"`
	Version  string    `json:"version,omitempty"`
	Addr     string    `json:"addr,omitempty"`
	LastSeen time.Time `json:"last_seen"`
}

// Registry tracks slaves that have recently checked in. Lookups and writes
// are concurrent-safe. Cleared on master restart — the slave's next /register
// call re-establishes presence.
type Registry struct {
	mu     sync.RWMutex
	slaves map[string]*SlaveInfo
}

func NewRegistry() *Registry {
	return &Registry{slaves: make(map[string]*SlaveInfo)}
}

// Touch records that a slave just checked in. Safe to call on every request
// that carries a valid slave identity, not just /register.
func (r *Registry) Touch(name, version, addr string) {
	if name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	info, ok := r.slaves[name]
	if !ok {
		info = &SlaveInfo{Name: name}
		r.slaves[name] = info
	}
	info.Version = version
	info.Addr = addr
	info.LastSeen = time.Now()
}

// Names returns the current set of registered slave names, sorted for stable
// UI rendering.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.slaves))
	for name := range r.slaves {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Has reports whether a slave has registered at least once since startup.
func (r *Registry) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.slaves[name]
	return ok
}

// Snapshot returns a copy of the current registry for debugging / UI.
func (r *Registry) Snapshot() []SlaveInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]SlaveInfo, 0, len(r.slaves))
	for _, v := range r.slaves {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
