package proxy

import (
	"testing"
	"time"
)

func TestStickyStatsNoRequests(t *testing.T) {
	s := NewStickyStats(5 * time.Minute)
	if got := s.Percent(); got != -1 {
		t.Errorf("Percent() = %v, want -1 (no requests)", got)
	}
}

func TestStickyStatsAllSticky(t *testing.T) {
	s := NewStickyStats(5 * time.Minute)
	for range 10 {
		s.Record(true)
	}
	if got := s.Percent(); got != 100 {
		t.Errorf("Percent() = %v, want 100", got)
	}
}

func TestStickyStatsNoneSticky(t *testing.T) {
	s := NewStickyStats(5 * time.Minute)
	for range 10 {
		s.Record(false)
	}
	if got := s.Percent(); got != 0 {
		t.Errorf("Percent() = %v, want 0", got)
	}
}

func TestStickyStatsMixed(t *testing.T) {
	s := NewStickyStats(5 * time.Minute)
	for range 3 {
		s.Record(true)
	}
	for range 7 {
		s.Record(false)
	}
	if got := s.Percent(); got != 30 {
		t.Errorf("Percent() = %v, want 30", got)
	}
}

func TestStickyStatsWindowExpiry(t *testing.T) {
	s := NewStickyStats(100 * time.Millisecond)
	s.Record(true)
	s.Record(true)

	// Wait for the window to expire.
	time.Sleep(150 * time.Millisecond)

	// After expiry, no events in window.
	if got := s.Percent(); got != -1 {
		t.Errorf("Percent() after expiry = %v, want -1", got)
	}

	// New non-sticky request should give 0%.
	s.Record(false)
	if got := s.Percent(); got != 0 {
		t.Errorf("Percent() after new request = %v, want 0", got)
	}
}
