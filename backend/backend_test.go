package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"vastproxy/vast"
)

func testInstance(id int) *vast.Instance {
	return &vast.Instance{
		ID:           id,
		JupyterToken: "test-token",
		PublicIPAddr: "1.2.3.4",
		SSHHost:      "ssh.example.com",
		SSHPort:      22,
	}
}

// --- Mock Tunnel ---

type mockTunnel struct {
	localAddr  string
	cmdOutput  string
	cmdErr     error
	closedFlag atomic.Bool
	direct     bool // whether this mock represents a direct SSH connection
}

func (m *mockTunnel) LocalAddr() string                     { return m.localAddr }
func (m *mockTunnel) RunCommand(cmd string) (string, error) { return m.cmdOutput, m.cmdErr }
func (m *mockTunnel) Close()                                { m.closedFlag.Store(true) }
func (m *mockTunnel) IsClosed() bool                        { return m.closedFlag.Load() }
func (m *mockTunnel) IsDirect() bool                        { return m.direct }

// mockTunnelFactory returns a TunnelFactory that produces the given mock.
func mockTunnelFactory(t Tunnel, err error) TunnelFactory {
	return func(publicIP string, directSSHPort int, sshHost string, sshPort int, keyPath string, remotePort int) (Tunnel, error) {
		return t, err
	}
}

// --- Basic tests ---

func TestNewBackend(t *testing.T) {
	inst := testInstance(1)
	be := NewBackend(inst, "", nil)
	if be.BaseURL() != "" {
		t.Errorf("BaseURL() = %q, want empty before tunnel", be.BaseURL())
	}
	if be.Token() != "test-token" {
		t.Errorf("Token() = %q", be.Token())
	}
}

func TestCheckHealthViaTunnel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %s, want /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	}))
	defer srv.Close()

	inst := testInstance(1)
	be := NewBackend(inst, "", nil)
	mock := &mockTunnel{localAddr: srv.Listener.Addr().String()}
	be.SetTunnel(mock)

	err := be.CheckHealth(context.Background())
	if err != nil {
		t.Fatalf("CheckHealth() error: %v", err)
	}
	if !be.IsHealthy() {
		t.Error("expected healthy after successful check")
	}
	want := fmt.Sprintf("http://%s", mock.localAddr)
	if be.BaseURL() != want {
		t.Errorf("BaseURL() = %q, want %q", be.BaseURL(), want)
	}
}

func TestCheckHealthTunnelFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	inst := testInstance(1)
	be := NewBackend(inst, "", nil)
	mock := &mockTunnel{localAddr: srv.Listener.Addr().String()}
	be.SetTunnel(mock)

	err := be.CheckHealth(context.Background())
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	if be.IsHealthy() {
		t.Error("should not be healthy after failed check")
	}
}

func TestCheckHealthNoTunnel(t *testing.T) {
	inst := testInstance(1)
	be := NewBackend(inst, "", nil)

	err := be.CheckHealth(context.Background())
	if err == nil {
		t.Fatal("expected error with no tunnel")
	}
}

// --- EnsureSSH tests ---

func TestEnsureSSHWithMockFactory(t *testing.T) {
	inst := testInstance(1)
	be := NewBackend(inst, "", nil)

	mock := &mockTunnel{localAddr: "127.0.0.1:55555"}
	be.SetTunnelFactory(mockTunnelFactory(mock, nil))

	got := be.EnsureSSH()
	if !got {
		t.Fatal("EnsureSSH() = false, want true")
	}

	// Second call should short-circuit (tunnel already exists).
	got = be.EnsureSSH()
	if !got {
		t.Fatal("second EnsureSSH() = false, want true")
	}
}

func TestEnsureSSHFactoryError(t *testing.T) {
	inst := testInstance(1)
	be := NewBackend(inst, "", nil)
	be.SetTunnelFactory(mockTunnelFactory(nil, fmt.Errorf("ssh connection refused")))

	got := be.EnsureSSH()
	if got {
		t.Fatal("EnsureSSH() = true, want false after error")
	}
	if be.sshFails != 1 {
		t.Errorf("sshFails = %d, want 1", be.sshFails)
	}
}

func TestEnsureSSHBackoff(t *testing.T) {
	inst := testInstance(1)
	be := NewBackend(inst, "", nil)
	be.SetTunnelFactory(mockTunnelFactory(nil, fmt.Errorf("connection refused")))

	// First failure.
	be.EnsureSSH()
	// Should be in backoff now.
	got := be.EnsureSSH()
	if got {
		t.Fatal("EnsureSSH() should return false during backoff")
	}
}

func TestEnsureSSHResetsFailCountOnSuccess(t *testing.T) {
	inst := testInstance(1)
	be := NewBackend(inst, "", nil)

	// First: fail a few times.
	be.SetTunnelFactory(mockTunnelFactory(nil, fmt.Errorf("fail")))
	be.EnsureSSH()
	be.sshBackoffTil = time.Time{} // clear backoff for test
	be.EnsureSSH()
	if be.sshFails != 2 {
		t.Fatalf("sshFails = %d, want 2", be.sshFails)
	}

	// Now succeed.
	mock := &mockTunnel{localAddr: "127.0.0.1:55555"}
	be.SetTunnelFactory(mockTunnelFactory(mock, nil))
	be.sshBackoffTil = time.Time{}
	got := be.EnsureSSH()
	if !got {
		t.Fatal("EnsureSSH() = false after setting good factory")
	}
	if be.sshFails != 0 {
		t.Errorf("sshFails = %d, want 0 after success", be.sshFails)
	}
}

// --- FetchGPUMetrics tests ---

func TestFetchGPUMetrics(t *testing.T) {
	inst := testInstance(1)
	be := NewBackend(inst, "", nil)

	mock := &mockTunnel{cmdOutput: "85, 72"}
	be.SetTunnel(mock)

	metrics, err := be.FetchGPUMetrics()
	if err != nil {
		t.Fatalf("FetchGPUMetrics() error: %v", err)
	}
	if len(metrics.GPUs) != 1 {
		t.Fatalf("GPUs len = %d, want 1", len(metrics.GPUs))
	}
	if metrics.GPUs[0].Utilization != 85 {
		t.Errorf("Utilization = %f, want 85", metrics.GPUs[0].Utilization)
	}
	if metrics.GPUs[0].Temperature != 72 {
		t.Errorf("Temperature = %f, want 72", metrics.GPUs[0].Temperature)
	}
}

func TestFetchGPUMetricsNoTunnel(t *testing.T) {
	inst := testInstance(1)
	be := NewBackend(inst, "", nil)

	_, err := be.FetchGPUMetrics()
	if err == nil {
		t.Fatal("expected error when no tunnel is set")
	}
}

func TestFetchGPUMetricsCommandError(t *testing.T) {
	inst := testInstance(1)
	be := NewBackend(inst, "", nil)

	mock := &mockTunnel{cmdErr: fmt.Errorf("session closed")}
	be.SetTunnel(mock)

	_, err := be.FetchGPUMetrics()
	if err == nil {
		t.Fatal("expected error from RunCommand failure")
	}
}

// --- Acquire/Release tests ---

func TestAcquireRelease(t *testing.T) {
	inst := testInstance(1)
	be := NewBackend(inst, "", nil)

	if be.ActiveRequests() != 0 {
		t.Fatalf("initial active = %d", be.ActiveRequests())
	}

	be.Acquire()
	be.Acquire()
	if be.ActiveRequests() != 2 {
		t.Errorf("after 2 acquires: %d", be.ActiveRequests())
	}

	be.Release()
	if be.ActiveRequests() != 1 {
		t.Errorf("after 1 release: %d", be.ActiveRequests())
	}
}

func TestAcquireReleaseConcurrent(t *testing.T) {
	inst := testInstance(1)
	be := NewBackend(inst, "", nil)

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			be.Acquire()
			be.Release()
		}()
	}
	wg.Wait()

	if be.ActiveRequests() != 0 {
		t.Errorf("after concurrent acquire/release: %d", be.ActiveRequests())
	}
}

// --- SetHealthy / Close tests ---

func TestSetHealthy(t *testing.T) {
	inst := testInstance(1)
	be := NewBackend(inst, "", nil)

	if be.IsHealthy() {
		t.Error("should start unhealthy")
	}
	be.SetHealthy(true)
	if !be.IsHealthy() {
		t.Error("should be healthy after SetHealthy(true)")
	}
	be.SetHealthy(false)
	if be.IsHealthy() {
		t.Error("should be unhealthy after SetHealthy(false)")
	}
}

func TestCloseWithoutTunnel(t *testing.T) {
	inst := testInstance(1)
	be := NewBackend(inst, "", nil)
	be.SetHealthy(true)

	be.Close()
	if be.IsHealthy() {
		t.Error("should be unhealthy after Close")
	}
}

func TestCloseWithTunnel(t *testing.T) {
	inst := testInstance(1)
	be := NewBackend(inst, "", nil)
	be.SetHealthy(true)

	mock := &mockTunnel{localAddr: "127.0.0.1:55555"}
	be.SetTunnel(mock)

	be.Close()
	if be.IsHealthy() {
		t.Error("should be unhealthy after Close")
	}
	if !mock.IsClosed() {
		t.Error("tunnel Close() was not called")
	}
}

// --- FetchModel tests ---

func TestFetchModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"id": "Qwen/Qwen3-VL-72B"}},
		})
	}))
	defer srv.Close()

	inst := testInstance(1)
	be := NewBackend(inst, "", nil)
	be.baseURL = srv.URL

	name, err := be.FetchModel(context.Background())
	if err != nil {
		t.Fatalf("FetchModel() error: %v", err)
	}
	if name != "Qwen/Qwen3-VL-72B" {
		t.Errorf("model = %q", name)
	}
}

func TestFetchModelNoModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	}))
	defer srv.Close()

	inst := testInstance(1)
	be := NewBackend(inst, "", nil)
	be.baseURL = srv.URL

	_, err := be.FetchModel(context.Background())
	if err == nil {
		t.Fatal("expected error for empty model list")
	}
}

func TestFetchModelNoBaseURL(t *testing.T) {
	inst := testInstance(1)
	be := NewBackend(inst, "", nil)

	_, err := be.FetchModel(context.Background())
	if err == nil {
		t.Fatal("expected error for empty baseURL")
	}
}

func TestHTTPClient(t *testing.T) {
	inst := testInstance(1)
	be := NewBackend(inst, "", nil)
	if be.HTTPClient() == nil {
		t.Fatal("HTTPClient() returned nil")
	}
}

// --- StartHealthLoop tests ---

func TestStartHealthLoopTransitionsToHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"id": "test-model"}},
		})
	}))
	defer srv.Close()

	// Set up watcher with the instance pre-registered.
	watcher := vast.NewWatcher(nil, time.Hour)
	watcher.InjectInstance(&vast.Instance{ID: 1, State: vast.StateConnecting})

	inst := testInstance(1)
	be := NewBackend(inst, "", nil)
	be.healthInterval = 10 * time.Millisecond
	mock := &mockTunnel{localAddr: srv.Listener.Addr().String(), cmdOutput: "50, 60"}
	be.SetTunnelFactory(mockTunnelFactory(mock, nil))

	gpuCh := make(chan GPUUpdate, 10)
	ctx, cancel := context.WithCancel(context.Background())

	go be.StartHealthLoop(ctx, watcher, gpuCh)

	// Wait for health loop to run at least once.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for backend to become healthy")
		case <-time.After(10 * time.Millisecond):
			if be.IsHealthy() {
				cancel()
				// Verify watcher got the state update.
				instances := watcher.Instances()
				if instances[1].State != vast.StateHealthy {
					t.Errorf("watcher state = %v, want HEALTHY", instances[1].State)
				}
				return
			}
		}
	}
}

func TestStartHealthLoopGPUMetrics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"id": "test-model"}},
		})
	}))
	defer srv.Close()

	watcher := vast.NewWatcher(nil, time.Hour)
	watcher.InjectInstance(&vast.Instance{ID: 1, State: vast.StateConnecting})

	inst := testInstance(1)
	be := NewBackend(inst, "", nil)
	be.healthInterval = 10 * time.Millisecond

	// Inject a mock tunnel that returns GPU metrics and points at the test server.
	mock := &mockTunnel{
		localAddr: srv.Listener.Addr().String(),
		cmdOutput: "92, 68",
	}
	be.SetTunnel(mock)
	be.SetTunnelFactory(mockTunnelFactory(mock, nil))

	gpuCh := make(chan GPUUpdate, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go be.StartHealthLoop(ctx, watcher, gpuCh)

	// Wait for a GPU update.
	select {
	case update := <-gpuCh:
		if update.InstanceID != 1 {
			t.Errorf("InstanceID = %d, want 1", update.InstanceID)
		}
		if len(update.GPUs) != 1 {
			t.Fatalf("GPUs len = %d, want 1", len(update.GPUs))
		}
		if update.GPUs[0].Utilization != 92 {
			t.Errorf("Utilization = %f, want 92", update.GPUs[0].Utilization)
		}
		if update.GPUs[0].Temperature != 68 {
			t.Errorf("Temperature = %f, want 68", update.GPUs[0].Temperature)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for GPU update")
	}
}

func TestStartHealthLoopTunnelBreakAndRecover(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "m"}}})
	}))
	defer srv.Close()

	watcher := vast.NewWatcher(nil, time.Hour)
	watcher.InjectInstance(&vast.Instance{ID: 1, State: vast.StateConnecting})

	inst := testInstance(1)
	be := NewBackend(inst, "", nil)
	be.healthInterval = 10 * time.Millisecond

	// Start with a tunnel that points at the server but returns GPU metric errors
	// (simulating a broken SSH session while health checks still succeed).
	brokenMock := &mockTunnel{
		localAddr: srv.Listener.Addr().String(),
		cmdErr:    fmt.Errorf("broken pipe"),
	}
	be.SetTunnel(brokenMock)

	gpuCh := make(chan GPUUpdate, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go be.StartHealthLoop(ctx, watcher, gpuCh)

	// Wait for health loop to close the broken tunnel.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for tunnel teardown")
		case <-time.After(10 * time.Millisecond):
			if brokenMock.IsClosed() {
				return // Success — tunnel was torn down.
			}
		}
	}
}

func TestStartHealthLoopUnhealthyThenRemoved(t *testing.T) {
	// Simulate a backend that fails health checks and is then removed from the watcher.
	// The health loop should exit when the instance is no longer tracked.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	watcher := vast.NewWatcher(nil, time.Hour)
	watcher.InjectInstance(&vast.Instance{ID: 1, State: vast.StateHealthy})

	inst := testInstance(1)
	be := NewBackend(inst, "", nil)
	be.healthInterval = 10 * time.Millisecond
	be.SetHealthy(true) // start healthy
	mock := &mockTunnel{localAddr: srv.Listener.Addr().String()}
	be.SetTunnel(mock)
	be.SetTunnelFactory(mockTunnelFactory(mock, nil))

	gpuCh := make(chan GPUUpdate, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		be.StartHealthLoop(ctx, watcher, gpuCh)
		close(done)
	}()

	// Wait for backend to become unhealthy (health check fails).
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for backend to become unhealthy")
		case <-time.After(10 * time.Millisecond):
			if !be.IsHealthy() {
				goto removeInstance
			}
		}
	}

removeInstance:
	// Mark instance as removing in the watcher so HasInstance returns false.
	watcher.SetInstanceState(1, vast.StateRemoving)

	// The health loop should exit because HasInstance(1) returns false.
	select {
	case <-done:
		// Good — loop exited after instance removal.
	case <-time.After(2 * time.Second):
		t.Fatal("StartHealthLoop didn't exit after instance removal")
	}
}

func TestFetchModelHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	inst := testInstance(1)
	be := NewBackend(inst, "", nil)
	be.baseURL = srv.URL

	_, err := be.FetchModel(context.Background())
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestFetchModelBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	inst := testInstance(1)
	be := NewBackend(inst, "", nil)
	be.baseURL = srv.URL

	_, err := be.FetchModel(context.Background())
	if err == nil {
		t.Fatal("expected error for invalid JSON response")
	}
}

func TestAbortAll(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	inst := testInstance(1)
	be := NewBackend(inst, "", nil)
	be.baseURL = srv.URL

	if err := be.AbortAll(context.Background()); err != nil {
		t.Fatalf("AbortAll() error: %v", err)
	}
	if gotPath != "/abort_request" {
		t.Errorf("path = %q, want /abort_request", gotPath)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("auth = %q, want Bearer test-token", gotAuth)
	}
	if gotBody != `{"rid":""}` {
		t.Errorf("body = %q, want {\"rid\":\"\"}", gotBody)
	}
}

func TestAbortAllHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	inst := testInstance(1)
	be := NewBackend(inst, "", nil)
	be.baseURL = srv.URL

	err := be.AbortAll(context.Background())
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestAbortAllNoBaseURL(t *testing.T) {
	inst := testInstance(1)
	be := NewBackend(inst, "", nil)

	err := be.AbortAll(context.Background())
	if err == nil {
		t.Fatal("expected error for empty base URL")
	}
}

func TestStartHealthLoopContextCancel(t *testing.T) {
	watcher := vast.NewWatcher(nil, time.Hour)
	watcher.InjectInstance(&vast.Instance{ID: 1, State: vast.StateConnecting})

	inst := testInstance(1)
	be := NewBackend(inst, "", nil)
	be.healthInterval = 10 * time.Millisecond
	be.SetTunnelFactory(mockTunnelFactory(nil, fmt.Errorf("no ssh")))

	gpuCh := make(chan GPUUpdate, 10)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		be.StartHealthLoop(ctx, watcher, gpuCh)
		close(done)
	}()

	// Cancel immediately and ensure the loop exits promptly.
	cancel()
	select {
	case <-done:
		// Good — loop exited.
	case <-time.After(1 * time.Second):
		t.Fatal("StartHealthLoop didn't exit after context cancel")
	}
}
