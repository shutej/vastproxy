package proxy

import (
	"fmt"
	"sync"
	"vastproxy/backend"
)

// Balancer implements least-connections load balancing across backends.
type Balancer struct {
	backends []*backend.Backend
	mu       sync.RWMutex
}

// NewBalancer creates a new load balancer.
func NewBalancer() *Balancer {
	return &Balancer{}
}

// SetBackends replaces the set of backends.
func (b *Balancer) SetBackends(backends []*backend.Backend) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.backends = backends
}

// ErrNoBackends is returned when no healthy backends are available.
var ErrNoBackends = fmt.Errorf("no healthy backends available")

// Pick selects the healthy backend with the fewest active requests.
func (b *Balancer) Pick() (*backend.Backend, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var best *backend.Backend
	var bestCount int64 = -1

	for _, be := range b.backends {
		if !be.IsHealthy() {
			continue
		}
		count := be.ActiveRequests()
		if best == nil || count < bestCount {
			best = be
			bestCount = count
		}
	}

	if best == nil {
		return nil, ErrNoBackends
	}
	return best, nil
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
