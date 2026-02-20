package vast

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestWatcherPollAddsAndRemoves(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var instances []Instance
		if callCount == 1 {
			instances = []Instance{
				{ID: 1, ActualStatus: "running", PublicIPAddr: "1.2.3.4",
					Ports: map[string][]PortMapping{"8000/tcp": {{HostPort: "12345"}}}},
				{ID: 2, ActualStatus: "running", PublicIPAddr: "5.6.7.8",
					Ports: map[string][]PortMapping{"8000/tcp": {{HostPort: "54321"}}}},
			}
		} else {
			// Second poll: instance 2 disappeared.
			instances = []Instance{
				{ID: 1, ActualStatus: "running", PublicIPAddr: "1.2.3.4",
					Ports: map[string][]PortMapping{"8000/tcp": {{HostPort: "12345"}}}},
			}
		}
		json.NewEncoder(w).Encode(InstancesResponse{Instances: instances})
	}))
	defer srv.Close()

	c := newTestClient("key", srv.URL)
	w := NewWatcher(c, 50*time.Millisecond)
	ch := w.Subscribe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Start(ctx)

	// Collect events.
	var events []InstanceEvent
	timeout := time.After(500 * time.Millisecond)
	for {
		select {
		case evt := <-ch:
			events = append(events, evt)
			// We expect: added(1), added(2), updated(1), removed(2)
			if len(events) >= 4 {
				cancel()
				goto check
			}
		case <-timeout:
			goto check
		}
	}
check:
	// Verify we got at least the two adds.
	addedIDs := map[int]bool{}
	removedIDs := map[int]bool{}
	for _, evt := range events {
		switch evt.Type {
		case "added":
			addedIDs[evt.Instance.ID] = true
		case "removed":
			removedIDs[evt.Instance.ID] = true
		}
	}
	if !addedIDs[1] || !addedIDs[2] {
		t.Errorf("expected adds for 1 and 2, got adds: %v", addedIDs)
	}
	if !removedIDs[2] {
		t.Errorf("expected remove for 2, got removes: %v", removedIDs)
	}
}

func TestWatcherSubscribeFanOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(InstancesResponse{
			Instances: []Instance{{ID: 99, ActualStatus: "running", PublicIPAddr: "1.1.1.1",
				Ports: map[string][]PortMapping{"8000/tcp": {{HostPort: "8000"}}}}},
		})
	}))
	defer srv.Close()

	c := newTestClient("key", srv.URL)
	w := NewWatcher(c, time.Hour) // long interval — we'll trigger poll manually

	ch1 := w.Subscribe()
	ch2 := w.Subscribe()

	ctx := context.Background()
	w.poll(ctx)

	// Both subscribers should get the "added" event.
	for i, ch := range []<-chan InstanceEvent{ch1, ch2} {
		select {
		case evt := <-ch:
			if evt.Type != "added" || evt.Instance.ID != 99 {
				t.Errorf("subscriber %d: got %s/%d, want added/99", i, evt.Type, evt.Instance.ID)
			}
		default:
			t.Errorf("subscriber %d: no event received", i)
		}
	}
}

func TestSetInstanceState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(InstancesResponse{
			Instances: []Instance{{ID: 1, ActualStatus: "running", PublicIPAddr: "1.1.1.1",
				Ports: map[string][]PortMapping{"8000/tcp": {{HostPort: "8000"}}}}},
		})
	}))
	defer srv.Close()

	c := newTestClient("key", srv.URL)
	w := NewWatcher(c, time.Hour)
	w.poll(context.Background())

	w.SetInstanceState(1, StateHealthy)

	instances := w.Instances()
	if instances[1].State != StateHealthy {
		t.Errorf("state = %v, want HEALTHY", instances[1].State)
	}
}

func TestHasInstance(t *testing.T) {
	w := NewWatcher(nil, time.Hour)
	w.instances[1] = &Instance{ID: 1, State: StateHealthy}
	w.instances[2] = &Instance{ID: 2, State: StateRemoving}

	if !w.HasInstance(1) {
		t.Error("HasInstance(1) = false, want true")
	}
	if w.HasInstance(2) {
		t.Error("HasInstance(2) = true, want false (removing)")
	}
	if w.HasInstance(99) {
		t.Error("HasInstance(99) = true, want false (not found)")
	}
}

func TestWatcherPollSkipsNonRunning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(InstancesResponse{
			Instances: []Instance{
				{ID: 1, ActualStatus: "running", PublicIPAddr: "1.1.1.1",
					Ports: map[string][]PortMapping{"8000/tcp": {{HostPort: "8000"}}}},
				{ID: 2, ActualStatus: "loading", PublicIPAddr: "2.2.2.2"},
				{ID: 3, ActualStatus: "exited", PublicIPAddr: "3.3.3.3"},
			},
		})
	}))
	defer srv.Close()

	c := newTestClient("key", srv.URL)
	w := NewWatcher(c, time.Hour)
	ch := w.Subscribe()

	w.poll(context.Background())

	// Only instance 1 (running) should be added.
	select {
	case evt := <-ch:
		if evt.Type != "added" || evt.Instance.ID != 1 {
			t.Errorf("got %s/%d, want added/1", evt.Type, evt.Instance.ID)
		}
	default:
		t.Fatal("expected event for instance 1")
	}

	// No more events (instances 2 and 3 should be skipped).
	select {
	case evt := <-ch:
		t.Errorf("unexpected event: %s/%d", evt.Type, evt.Instance.ID)
	default:
		// Good.
	}

	instances := w.Instances()
	if len(instances) != 1 {
		t.Errorf("expected 1 tracked instance, got %d", len(instances))
	}
}

func TestWatcherPollUpdatesExisting(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		util := float64(50 + callCount*10)
		json.NewEncoder(w).Encode(InstancesResponse{
			Instances: []Instance{
				{ID: 1, ActualStatus: "running", PublicIPAddr: "1.1.1.1",
					GPUUtil: &util,
					Ports:   map[string][]PortMapping{"8000/tcp": {{HostPort: "8000"}}}},
			},
		})
	}))
	defer srv.Close()

	c := newTestClient("key", srv.URL)
	w := NewWatcher(c, time.Hour)
	ch := w.Subscribe()

	// First poll — added.
	w.poll(context.Background())
	evt := <-ch
	if evt.Type != "added" {
		t.Fatalf("first event type = %s, want added", evt.Type)
	}

	// Second poll — updated (same instance, different GPU util).
	w.poll(context.Background())
	evt = <-ch
	if evt.Type != "updated" {
		t.Fatalf("second event type = %s, want updated", evt.Type)
	}
	if evt.Instance.ID != 1 {
		t.Errorf("instance ID = %d, want 1", evt.Instance.ID)
	}
}

func TestWatcherPollError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient("key", srv.URL)
	w := NewWatcher(c, time.Hour)
	ch := w.Subscribe()

	// Poll should handle error gracefully without crashing.
	w.poll(context.Background())

	// No events should be emitted.
	select {
	case evt := <-ch:
		t.Errorf("unexpected event on poll error: %s/%d", evt.Type, evt.Instance.ID)
	default:
		// Good.
	}
}

func TestInjectInstance(t *testing.T) {
	w := NewWatcher(nil, time.Hour)
	inst := &Instance{ID: 42, State: StateHealthy, PublicIPAddr: "10.0.0.1"}
	w.InjectInstance(inst)

	if !w.HasInstance(42) {
		t.Error("HasInstance(42) = false after InjectInstance")
	}
	instances := w.Instances()
	if instances[42].PublicIPAddr != "10.0.0.1" {
		t.Errorf("PublicIPAddr = %q, want 10.0.0.1", instances[42].PublicIPAddr)
	}
}

func TestSetInstanceStateNonExistent(t *testing.T) {
	w := NewWatcher(nil, time.Hour)
	// Setting state on non-existent instance should not panic.
	w.SetInstanceState(999, StateHealthy)
	if w.HasInstance(999) {
		t.Error("non-existent instance should not appear after SetInstanceState")
	}
}

func TestHasInstanceRemoved(t *testing.T) {
	w := NewWatcher(nil, time.Hour)
	w.instances[1] = &Instance{ID: 1, State: StateRemoved}

	if w.HasInstance(1) {
		t.Error("HasInstance(1) = true, want false for REMOVED state")
	}
}

func TestWatcherConcurrentAccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(InstancesResponse{
			Instances: []Instance{{ID: 1, ActualStatus: "running", PublicIPAddr: "1.1.1.1",
				Ports: map[string][]PortMapping{"8000/tcp": {{HostPort: "8000"}}}}},
		})
	}))
	defer srv.Close()

	c := newTestClient("key", srv.URL)
	w := NewWatcher(c, time.Hour)
	w.Subscribe()
	w.poll(context.Background())

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(3)
		go func() { defer wg.Done(); w.HasInstance(1) }()
		go func() { defer wg.Done(); w.Instances() }()
		go func() { defer wg.Done(); w.SetInstanceState(1, StateHealthy) }()
	}
	wg.Wait()
}
