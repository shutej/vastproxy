package proxy

import (
	"fmt"
	"log"
	"sync"
	"vastproxy/backend"
)

// Balancer implements least-connections load balancing across backends.
type Balancer struct {
	backends   []*backend.Backend
	lastPicked int // index of last picked backend for round-robin tie-breaking
	mu         sync.Mutex
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
// When multiple backends have the same load, round-robins among them.
func (b *Balancer) Pick() (*backend.Backend, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n := len(b.backends)
	if n == 0 {
		return nil, ErrNoBackends
	}

	var best *backend.Backend
	var bestIdx int
	var bestCount int64 = -1

	// Start scanning from one past the last picked index so that
	// equal-load backends get rotated through round-robin style.
	start := (b.lastPicked + 1) % n
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		be := b.backends[idx]
		if !be.IsHealthy() {
			continue
		}
		count := be.ActiveRequests()
		if best == nil || count < bestCount {
			best = be
			bestIdx = idx
			bestCount = count
		}
	}

	if best == nil {
		return nil, ErrNoBackends
	}
	b.lastPicked = bestIdx
	log.Printf("balancer: picked instance %d (idx=%d, active=%d, total_backends=%d)",
		best.Instance.ID, bestIdx, bestCount, n)
	return best, nil
}

// HealthyCount returns the number of healthy backends.
func (b *Balancer) HealthyCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
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
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.backends)
}
