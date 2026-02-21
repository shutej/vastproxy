package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"vastproxy/backend"
	"vastproxy/vast"
)

func makeBackend(id int, healthy bool) *backend.Backend {
	inst := &vast.Instance{ID: id, JupyterToken: "tok"}
	be := backend.NewBackend(inst, "", nil)
	be.SetBaseURL("http://localhost/v1")
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

func TestPickDynamicHealthTransition(t *testing.T) {
	bal := NewBalancer()
	b1 := makeBackend(1, true)
	b2 := makeBackend(2, true)
	b3 := makeBackend(3, true)
	bal.SetBackends([]*backend.Backend{b1, b2, b3})

	// All 3 healthy — should distribute across all 3.
	ids := map[int]int{}
	for range 6 {
		be, _ := bal.Pick()
		ids[be.Instance.ID]++
	}
	if len(ids) != 3 {
		t.Fatalf("expected 3 backends, got %v", ids)
	}

	// Backend 2 becomes unhealthy (e.g. health check failed).
	b2.SetHealthy(false)

	// Now only 1 and 3 should be picked — no SetBackends needed.
	ids = map[int]int{}
	for range 6 {
		be, _ := bal.Pick()
		ids[be.Instance.ID]++
	}
	if ids[2] > 0 {
		t.Errorf("unhealthy backend 2 was picked %d times", ids[2])
	}
	if len(ids) != 2 || ids[1] != 3 || ids[3] != 3 {
		t.Errorf("expected {1:3, 3:3}, got %v", ids)
	}

	// Backend 2 recovers.
	b2.SetHealthy(true)

	ids = map[int]int{}
	for range 9 {
		be, _ := bal.Pick()
		ids[be.Instance.ID]++
	}
	if len(ids) != 3 {
		t.Errorf("expected 3 backends after recovery, got %v", ids)
	}
	for id, c := range ids {
		if c != 3 {
			t.Errorf("backend %d picked %d times, want 3", id, c)
		}
	}
}

func TestPickAllBecomeUnhealthy(t *testing.T) {
	bal := NewBalancer()
	b1 := makeBackend(1, true)
	b2 := makeBackend(2, true)
	bal.SetBackends([]*backend.Backend{b1, b2})

	// Both healthy — picks work.
	_, err := bal.Pick()
	if err != nil {
		t.Fatal(err)
	}

	// Both become unhealthy.
	b1.SetHealthy(false)
	b2.SetHealthy(false)

	_, err = bal.Pick()
	if err != ErrNoBackends {
		t.Errorf("Pick() err = %v, want ErrNoBackends", err)
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

func TestBalancerAcquireRelease(t *testing.T) {
	bal := NewBalancer()

	if got := bal.ActiveRequests(); got != 0 {
		t.Fatalf("initial ActiveRequests = %d, want 0", got)
	}

	bal.Acquire()
	bal.Acquire()
	if got := bal.ActiveRequests(); got != 2 {
		t.Fatalf("after 2 acquires, ActiveRequests = %d, want 2", got)
	}

	remaining := bal.Release()
	if remaining != 1 {
		t.Fatalf("Release returned %d, want 1", remaining)
	}

	remaining = bal.Release()
	if remaining != 0 {
		t.Fatalf("Release returned %d, want 0", remaining)
	}
}

func TestBalancerAbortAll(t *testing.T) {
	var abortCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/abort_request" {
			abortCount.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	bal := NewBalancer()
	b1 := &vast.Instance{ID: 1, JupyterToken: "tok"}
	b2 := &vast.Instance{ID: 2, JupyterToken: "tok"}
	be1 := backend.NewBackend(b1, "", nil)
	be2 := backend.NewBackend(b2, "", nil)
	be1.SetBaseURL(srv.URL + "/v1")
	be2.SetBaseURL(srv.URL + "/v1")
	be1.SetHealthy(true)
	be2.SetHealthy(true)
	bal.SetBackends([]*backend.Backend{be1, be2})

	bal.AbortAll(context.Background())

	if got := abortCount.Load(); got != 2 {
		t.Errorf("abort called %d times, want 2", got)
	}
}

func TestBalancerAbortAllSkipsUnhealthy(t *testing.T) {
	var abortCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/abort_request" {
			abortCount.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	bal := NewBalancer()
	b1 := &vast.Instance{ID: 1, JupyterToken: "tok"}
	b2 := &vast.Instance{ID: 2, JupyterToken: "tok"}
	be1 := backend.NewBackend(b1, "", nil)
	be2 := backend.NewBackend(b2, "", nil)
	be1.SetBaseURL(srv.URL + "/v1")
	be2.SetBaseURL(srv.URL + "/v1")
	be1.SetHealthy(true)
	be2.SetHealthy(false) // unhealthy — should be skipped
	bal.SetBackends([]*backend.Backend{be1, be2})

	bal.AbortAll(context.Background())

	if got := abortCount.Load(); got != 1 {
		t.Errorf("abort called %d times, want 1 (unhealthy skipped)", got)
	}
}
