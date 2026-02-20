package proxy

import (
	"sync"
	"testing"
	"vastproxy/backend"
	"vastproxy/vast"
)

func makeBackend(id int, healthy bool) *backend.Backend {
	inst := &vast.Instance{ID: id, BaseURL: "http://localhost/v1", JupyterToken: "tok"}
	be := backend.NewBackend(inst, "", nil)
	if healthy {
		be.SetHealthy(true)
	}
	return be
}

func TestPickRoundRobin(t *testing.T) {
	bal := NewBalancer()
	b1 := makeBackend(1, true)
	b2 := makeBackend(2, true)
	b3 := makeBackend(3, true)
	bal.SetBackends([]*backend.Backend{b3, b1, b2}) // unsorted on purpose

	seen := map[int]int{}
	for i := 0; i < 9; i++ {
		be, err := bal.Pick()
		if err != nil {
			t.Fatalf("Pick() error: %v", err)
		}
		seen[be.Instance.ID]++
	}
	// Each backend should be picked exactly 3 times.
	for _, id := range []int{1, 2, 3} {
		if seen[id] != 3 {
			t.Errorf("backend %d picked %d times, want 3", id, seen[id])
		}
	}
}

func TestPickStableOrder(t *testing.T) {
	bal := NewBalancer()
	// Set backends in reverse order — should be sorted by ID internally.
	b1 := makeBackend(10, true)
	b2 := makeBackend(20, true)
	bal.SetBackends([]*backend.Backend{b2, b1})

	be, _ := bal.Pick()
	if be.Instance.ID != 10 {
		t.Errorf("first pick got ID %d, want 10", be.Instance.ID)
	}
	be, _ = bal.Pick()
	if be.Instance.ID != 20 {
		t.Errorf("second pick got ID %d, want 20", be.Instance.ID)
	}
}

func TestPickNoBackends(t *testing.T) {
	bal := NewBalancer()
	_, err := bal.Pick()
	if err != ErrNoBackends {
		t.Errorf("Pick() err = %v, want ErrNoBackends", err)
	}
}

func TestPickAllUnhealthy(t *testing.T) {
	bal := NewBalancer()
	bal.SetBackends([]*backend.Backend{makeBackend(1, false), makeBackend(2, false)})
	_, err := bal.Pick()
	if err != ErrNoBackends {
		t.Errorf("Pick() err = %v, want ErrNoBackends", err)
	}
}

func TestPickSkipsUnhealthy(t *testing.T) {
	bal := NewBalancer()
	bal.SetBackends([]*backend.Backend{
		makeBackend(1, false),
		makeBackend(2, true),
		makeBackend(3, false),
	})
	for i := 0; i < 5; i++ {
		be, err := bal.Pick()
		if err != nil {
			t.Fatalf("Pick() error: %v", err)
		}
		if be.Instance.ID != 2 {
			t.Errorf("Pick() got ID %d, want 2", be.Instance.ID)
		}
	}
}

func TestHealthyAndTotalCount(t *testing.T) {
	bal := NewBalancer()
	bal.SetBackends([]*backend.Backend{
		makeBackend(1, true),
		makeBackend(2, false),
		makeBackend(3, true),
	})
	if got := bal.HealthyCount(); got != 2 {
		t.Errorf("HealthyCount() = %d, want 2", got)
	}
	if got := bal.TotalCount(); got != 3 {
		t.Errorf("TotalCount() = %d, want 3", got)
	}
}

func TestPickConcurrent(t *testing.T) {
	bal := NewBalancer()
	bal.SetBackends([]*backend.Backend{
		makeBackend(1, true),
		makeBackend(2, true),
		makeBackend(3, true),
	})

	var wg sync.WaitGroup
	seen := make([]int, 100)
	for i := 0; i < 100; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			be, err := bal.Pick()
			if err != nil {
				t.Errorf("Pick() error: %v", err)
				return
			}
			seen[i] = be.Instance.ID
		}()
	}
	wg.Wait()

	counts := map[int]int{}
	for _, id := range seen {
		if id != 0 {
			counts[id]++
		}
	}
	// With 100 requests across 3 backends, each should get ~33.
	for id, c := range counts {
		if c < 20 || c > 50 {
			t.Errorf("backend %d got %d requests, expected ~33", id, c)
		}
	}
}

func TestSetBackendsResetsCleanly(t *testing.T) {
	bal := NewBalancer()
	bal.SetBackends([]*backend.Backend{makeBackend(1, true)})
	be, _ := bal.Pick()
	if be.Instance.ID != 1 {
		t.Fatalf("got %d", be.Instance.ID)
	}

	// Replace with different backends.
	bal.SetBackends([]*backend.Backend{makeBackend(5, true), makeBackend(6, true)})
	ids := map[int]bool{}
	for i := 0; i < 4; i++ {
		be, _ := bal.Pick()
		ids[be.Instance.ID] = true
	}
	if !ids[5] || !ids[6] {
		t.Errorf("expected both 5 and 6, got %v", ids)
	}
}
