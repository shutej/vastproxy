package proxy

import (
	"sync"
	"time"
)

// StickyStats tracks the percentage of requests that present the
// X-VastProxy-Instance header over a sliding time window.
type StickyStats struct {
	mu     sync.Mutex
	window time.Duration
	events []reqEvent
}

type reqEvent struct {
	at     time.Time
	sticky bool
}

// NewStickyStats creates a StickyStats with the given sliding window duration.
func NewStickyStats(window time.Duration) *StickyStats {
	return &StickyStats{window: window}
}

// Record records a request, noting whether it had the sticky header.
func (s *StickyStats) Record(sticky bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, reqEvent{at: time.Now(), sticky: sticky})
	s.pruneOlderThan(time.Now().Add(-s.window))
}

// Percent returns the percentage of requests with the sticky header
// over the sliding window. Returns -1 if no requests have been recorded.
func (s *StickyStats) Percent() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneOlderThan(time.Now().Add(-s.window))

	if len(s.events) == 0 {
		return -1
	}
	n := 0
	for _, e := range s.events {
		if e.sticky {
			n++
		}
	}
	return float64(n) / float64(len(s.events)) * 100
}

// pruneOlderThan removes events before cutoff. Must be called with mu held.
func (s *StickyStats) pruneOlderThan(cutoff time.Time) {
	i := 0
	for i < len(s.events) && s.events[i].at.Before(cutoff) {
		i++
	}
	if i > 0 {
		s.events = s.events[i:]
	}
}
