package proxy

import (
	"fmt"
	"log"
	"sort"
	"sync"
	"sync/atomic"
	"vastproxy/backend"
)

// Balancer implements round-robin load balancing across healthy backends.
type Balancer struct {
	backends []*backend.Backend
	counter  atomic.Uint64 // monotonically increasing request counter
	mu       sync.RWMutex
}

// NewBalancer creates a new load balancer.
func NewBalancer() *Balancer {
	return &Balancer{}
}

// SetBackends replaces the set of backends, sorted by instance ID for
// stable ordering (Go map iteration is random).
func (b *Balancer) SetBackends(backends []*backend.Backend) {
	sort.Slice(backends, func(i, j int) bool {
		return backends[i].Instance.ID < backends[j].Instance.ID
	})
	b.mu.Lock()
	defer b.mu.Unlock()
	b.backends = backends
}

// ErrNoBackends is returned when no healthy backends are available.
var ErrNoBackends = fmt.Errorf("no healthy backends available")

// Pick selects the next healthy backend using round-robin.
// The atomic counter ensures even distribution regardless of timing.
func (b *Balancer) Pick() (*backend.Backend, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	n := len(b.backends)
	if n == 0 {
		return nil, ErrNoBackends
	}

	// Collect healthy backends.
	healthy := make([]*backend.Backend, 0, n)
	for _, be := range b.backends {
		if be.IsHealthy() {
			healthy = append(healthy, be)
		}
	}

	if len(healthy) == 0 {
		return nil, ErrNoBackends
	}

	// Atomically increment and pick based on counter mod healthy count.
	idx := b.counter.Add(1) - 1
	pick := healthy[idx%uint64(len(healthy))]

	log.Printf("balancer: picked instance %d (counter=%d, healthy=%d/%d)",
		pick.Instance.ID, idx, len(healthy), n)
	return pick, nil
}

// HealthyCount returns the number of healthy backends.
func (b *Balancer) HealthyCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	n := 0
	for _, be := range b.backends {
		if be.IsHealthy() {
			n++
		}
	}
	return n
}

// TotalCount returns the total number of backends.
func (b *Balancer) TotalCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.backends)
}
