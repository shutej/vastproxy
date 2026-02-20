package vast

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// Watcher polls the vast.ai API and tracks instance lifecycle.
type Watcher struct {
	client       *Client
	pollInterval time.Duration
	instances    map[int]*Instance
	subscribers  []chan InstanceEvent
	mu           sync.RWMutex
}

// NewWatcher creates a new instance watcher.
func NewWatcher(client *Client, pollInterval time.Duration) *Watcher {
	return &Watcher{
		client:       client,
		pollInterval: pollInterval,
		instances:    make(map[int]*Instance),
	}
}

// Subscribe returns a new channel that receives a copy of every instance event.
// Each subscriber gets its own independent channel. Call before Start.
func (w *Watcher) Subscribe() <-chan InstanceEvent {
	ch := make(chan InstanceEvent, 64)
	w.subscribers = append(w.subscribers, ch)
	return ch
}

// Instances returns a snapshot of all tracked instances.
func (w *Watcher) Instances() map[int]*Instance {
	w.mu.RLock()
	defer w.mu.RUnlock()
	cp := make(map[int]*Instance, len(w.instances))
	for k, v := range w.instances {
		cp[k] = v
	}
	return cp
}

// HasInstance checks whether an instance ID is still known.
func (w *Watcher) HasInstance(id int) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	inst, ok := w.instances[id]
	return ok && inst.State != StateRemoving
}

// Start begins polling in the foreground. Call in a goroutine.
func (w *Watcher) Start(ctx context.Context) {
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	// Immediate first poll.
	w.poll(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.poll(ctx)
		}
	}
}

func (w *Watcher) poll(ctx context.Context) {
	instances, err := w.client.ListInstances(ctx)
	if err != nil {
		log.Printf("vast watcher: poll error: %v", err)
		return
	}

	log.Printf("vast watcher: poll returned %d instances", len(instances))

	w.mu.Lock()
	defer w.mu.Unlock()

	// Build set of current API instance IDs.
	seen := make(map[int]bool, len(instances))
	for i := range instances {
		inst := &instances[i]
		if inst.ActualStatus != "running" {
			log.Printf("vast watcher: instance %d status=%q (skipping)", inst.ID, inst.ActualStatus)
			continue
		}
		seen[inst.ID] = true

		existing, ok := w.instances[inst.ID]
		if !ok {
			// New instance.
			inst.ContainerPort = inst.ResolveContainerPort()
			inst.HostPort = inst.ResolveHostPort()
			inst.DirectSSHPort = inst.ResolveDirectSSHPort()
			inst.State = StateDiscovered
			inst.StateChangedAt = time.Now()
			if inst.PublicIPAddr != "" && inst.HostPort != 0 {
				inst.BaseURL = fmt.Sprintf("http://%s:%d/v1", inst.PublicIPAddr, inst.HostPort)
			}
			log.Printf("vast watcher: new instance %d: publicIP=%s hostPort=%d containerPort=%d directSSH=%d baseURL=%s ssh=%s:%d",
				inst.ID, inst.PublicIPAddr, inst.HostPort, inst.ContainerPort, inst.DirectSSHPort, inst.BaseURL, inst.SSHHost, inst.SSHPort)
			w.instances[inst.ID] = inst
			w.emit(InstanceEvent{Type: "added", Instance: inst})
		} else {
			// Update mutable fields (GPU metrics, status).
			existing.GPUUtil = inst.GPUUtil
			existing.GPUTemp = inst.GPUTemp
			existing.ActualStatus = inst.ActualStatus
			w.emit(InstanceEvent{Type: "updated", Instance: existing})
		}
	}

	// Detect removed instances.
	for id, inst := range w.instances {
		if !seen[id] && inst.State != StateRemoving {
			inst.State = StateRemoving
			inst.StateChangedAt = time.Now()
			w.emit(InstanceEvent{Type: "removed", Instance: inst})
		}
	}
}

func (w *Watcher) emit(evt InstanceEvent) {
	for _, ch := range w.subscribers {
		select {
		case ch <- evt:
		default:
			// Drop if channel full; subscriber will catch up.
		}
	}
}

// InjectInstance adds an instance directly (used in tests).
func (w *Watcher) InjectInstance(inst *Instance) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.instances[inst.ID] = inst
}

// SetInstanceState updates an instance's state (called from backend manager).
func (w *Watcher) SetInstanceState(id int, state InstanceState) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if inst, ok := w.instances[id]; ok {
		inst.State = state
		inst.StateChangedAt = time.Now()
	}
}
