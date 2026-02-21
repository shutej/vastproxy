package proxy

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/shutej/vastproxy/backend"
)

// Balancer implements round-robin load balancing across healthy backends.
type Balancer struct {
	backends   []*backend.Backend
	counter    atomic.Uint64 // monotonically increasing request counter
	activeReqs atomic.Int64  // total in-flight requests across all backends
	mu         sync.RWMutex
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

// PickByID selects a specific backend by instance ID.
// Returns ErrNoBackends if the instance doesn't exist or isn't healthy.
func (b *Balancer) PickByID(id int) (*backend.Backend, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, be := range b.backends {
		if be.Instance.ID == id && be.IsHealthy() {
			return be, nil
		}
	}
	return nil, ErrNoBackends
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

// Acquire increments the global active request counter and returns the new value.
func (b *Balancer) Acquire() int64 {
	return b.activeReqs.Add(1)
}

// Release decrements the global active request counter and returns the new value.
func (b *Balancer) Release() int64 {
	return b.activeReqs.Add(-1)
}

// ActiveRequests returns the total number of in-flight requests.
func (b *Balancer) ActiveRequests() int64 {
	return b.activeReqs.Load()
}

// HasAbortSupport reports whether any backend supports server-side abort.
func (b *Balancer) HasAbortSupport() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, be := range b.backends {
		if be.Instance.Engine.SupportsAbort() {
			return true
		}
	}
	return false
}

// AbortAll sends abort requests to all healthy backends that support it.
func (b *Balancer) AbortAll(ctx context.Context) {
	b.mu.RLock()
	backends := make([]*backend.Backend, len(b.backends))
	copy(backends, b.backends)
	b.mu.RUnlock()

	for _, be := range backends {
		if be.IsHealthy() {
			if err := be.AbortAll(ctx); err != nil {
				log.Printf("balancer: abort on backend %d failed: %v", be.Instance.ID, err)
			} else {
				log.Printf("balancer: aborted all requests on backend %d", be.Instance.ID)
			}
		}
	}
}
