package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"vastproxy/vast"
)

// Backend represents a single SGLang backend instance.
// All HTTP traffic is routed through an SSH tunnel — no direct HTTP to instances.
type Backend struct {
	Instance           *vast.Instance
	baseURL            string // tunnel URL set by CheckHealth (e.g. "http://127.0.0.1:PORT")
	httpClient         *http.Client
	tunnel             Tunnel
	tunnelFactory      TunnelFactory // creates tunnels; nil = use NewSSHTunnel
	activeReqs         atomic.Int64
	healthy            atomic.Bool
	keyPath            string
	vastClient         *vast.Client
	healthInterval     time.Duration // tick interval for StartHealthLoop; 0 = 5s
	sshFails           int           // consecutive SSH tunnel creation failures
	sshBackoffTil      time.Time     // don't retry SSH until this time
	lastUpgradeAttempt time.Time     // last time we tried to upgrade proxy→direct SSH
	label              string        // managed label value; empty = labeling disabled
}

// NewBackend creates a backend for the given instance.
// label is the vast.ai label to apply when healthy; empty disables labeling.
func NewBackend(inst *vast.Instance, keyPath string, vastClient *vast.Client, label string) *Backend {
	return &Backend{
		Instance: inst,
		httpClient: &http.Client{
			Timeout: 5 * time.Minute,
		},
		keyPath:    keyPath,
		vastClient: vastClient,
		label:      label,
	}
}

// Token returns the instance's jupyter_token for Bearer auth on proxied ports.
func (b *Backend) Token() string {
	return b.Instance.JupyterToken
}

// BaseURL returns the root URL to reach this backend (no /v1 suffix).
func (b *Backend) BaseURL() string {
	return b.baseURL
}

// HTTPClient returns the HTTP client for raw requests (streaming).
func (b *Backend) HTTPClient() *http.Client {
	return b.httpClient
}

// ActiveRequests returns the number of in-flight requests.
func (b *Backend) ActiveRequests() int64 {
	return b.activeReqs.Load()
}

// Acquire increments the active request counter.
func (b *Backend) Acquire() {
	b.activeReqs.Add(1)
}

// Release decrements the active request counter.
func (b *Backend) Release() {
	b.activeReqs.Add(-1)
}

// IsHealthy returns whether this backend can serve requests.
func (b *Backend) IsHealthy() bool {
	return b.healthy.Load()
}

// SetHealthy sets the healthy state directly (used in tests).
func (b *Backend) SetHealthy(v bool) {
	b.healthy.Store(v)
}

// SetBaseURL sets the base URL directly (used in tests).
func (b *Backend) SetBaseURL(url string) {
	b.baseURL = url
}

// SetTunnel injects a tunnel (used in tests with mock tunnels).
func (b *Backend) SetTunnel(t Tunnel) {
	b.tunnel = t
}

// SetTunnelFactory injects a tunnel factory (used in tests).
func (b *Backend) SetTunnelFactory(f TunnelFactory) {
	b.tunnelFactory = f
}

// IsDirect returns true if the current SSH tunnel is a direct connection.
func (b *Backend) IsDirect() bool {
	if b.tunnel == nil {
		return false
	}
	return b.tunnel.IsDirect()
}

// CheckHealth verifies connectivity to the backend via the SSH tunnel.
// All HTTP traffic goes through the tunnel — no direct HTTP to instances.
func (b *Backend) CheckHealth(ctx context.Context) error {
	if b.tunnel == nil {
		b.healthy.Store(false)
		return fmt.Errorf("no tunnel for instance %d", b.Instance.ID)
	}

	tunnelURL := fmt.Sprintf("http://%s", b.tunnel.LocalAddr())
	if err := b.httpHealthCheck(ctx, tunnelURL); err != nil {
		b.healthy.Store(false)
		return fmt.Errorf("tunnel %s: %w", tunnelURL, err)
	}

	b.baseURL = tunnelURL
	b.healthy.Store(true)
	log.Printf("backend %d: health OK via tunnel %s", b.Instance.ID, tunnelURL)
	return nil
}

// EnsureSSH establishes an SSH tunnel for all HTTP traffic and GPU metrics.
// Returns true if SSH is available.
func (b *Backend) EnsureSSH() bool {
	if b.tunnel != nil {
		return true
	}
	if time.Now().Before(b.sshBackoffTil) {
		return false
	}
	factory := b.tunnelFactory
	if factory == nil {
		factory = NewSSHTunnel
	}
	tunnel, err := factory(
		b.Instance.PublicIPAddr,
		b.Instance.DirectSSHPort,
		b.Instance.SSHHost,
		b.Instance.SSHPort,
		b.keyPath,
		b.Instance.ContainerPort,
	)
	if err != nil {
		b.sshFails++

		// Exponential backoff: 10s, 20s, 40s, ... capped at 5m.
		wait := min(time.Duration(10<<min(b.sshFails-1, 5))*time.Second, 5*time.Minute)
		b.sshBackoffTil = time.Now().Add(wait)
		log.Printf("backend %d: ssh failed (%d consecutive), next retry in %v: %v",
			b.Instance.ID, b.sshFails, wait, err)
		return false
	}
	b.sshFails = 0
	b.tunnel = tunnel
	return true
}

func (b *Backend) httpHealthCheck(ctx context.Context, baseURL string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", baseURL+"/v1/models", nil)
	if err != nil {
		return err
	}
	if b.Instance.JupyterToken != "" {
		req.Header.Set("Authorization", "Bearer "+b.Instance.JupyterToken)
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned %d", resp.StatusCode)
	}
	return nil
}

// FetchGPUMetrics retrieves GPU metrics via SSH.
func (b *Backend) FetchGPUMetrics() (*GPUMetrics, error) {
	if b.tunnel == nil {
		return nil, fmt.Errorf("no ssh connection")
	}
	output, err := b.tunnel.RunCommand(
		"nvidia-smi --query-gpu=utilization.gpu,temperature.gpu --format=csv,noheader,nounits 2>/dev/null",
	)
	if err != nil {
		return nil, err
	}
	return ParseNvidiaSmi(output)
}

// FetchModel queries the backend's /v1/models endpoint and returns the first model name.
func (b *Backend) FetchModel(ctx context.Context) (string, error) {
	if b.baseURL == "" {
		return "", fmt.Errorf("no base URL")
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", b.baseURL+"/v1/models", nil)
	if err != nil {
		return "", err
	}
	if b.Instance.JupyterToken != "" {
		req.Header.Set("Authorization", "Bearer "+b.Instance.JupyterToken)
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// Parse just enough to get the model ID.
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if len(result.Data) > 0 {
		return result.Data[0].ID, nil
	}
	return "", fmt.Errorf("no models returned")
}

// StartHealthLoop periodically checks health and fetches GPU metrics.
// Call in a goroutine.
func (b *Backend) StartHealthLoop(ctx context.Context, watcher *vast.Watcher, gpuCh chan<- GPUUpdate) {
	interval := b.healthInterval
	if interval == 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	wasHealthy := b.healthy.Load()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// SSH tunnel is required for all HTTP traffic and GPU metrics.
			b.EnsureSSH()

			if err := b.CheckHealth(ctx); err != nil {
				log.Printf("backend %d: health check failed (wasHealthy=%v): %v", b.Instance.ID, wasHealthy, err)

				if wasHealthy {
					watcher.SetInstanceState(b.Instance.ID, vast.StateUnhealthy)
					go b.clearLabelIfOurs(ctx)
					wasHealthy = false
				}

				if !watcher.HasInstance(b.Instance.ID) {
					log.Printf("backend %d: removed from vast.ai", b.Instance.ID)
					b.healthy.Store(false)
					return
				}
				continue
			}

			if !wasHealthy {
				log.Printf("backend %d: now healthy", b.Instance.ID)
				watcher.SetInstanceState(b.Instance.ID, vast.StateHealthy)
				wasHealthy = true
				go b.setLabel(ctx, b.label)
			}

			// Discover model name if not yet known.
			if b.Instance.ModelName == "" {
				if name, err := b.FetchModel(ctx); err == nil {
					log.Printf("backend %d: model=%s", b.Instance.ID, name)
					b.Instance.ModelName = name
				}
			}

			// Attempt to upgrade proxy→direct SSH every 30s.
			b.tryUpgradeToDirect(ctx)

			// Fetch GPU metrics via SSH.
			if b.tunnel != nil {
				if metrics, err := b.FetchGPUMetrics(); err == nil {
					select {
					case gpuCh <- GPUUpdate{
						InstanceID: b.Instance.ID,
						GPUs:       metrics.GPUs,
						IsDirect:   b.tunnel.IsDirect(),
					}:
					default:
					}
				} else {
					// SSH session broke — tear down so EnsureSSH retries next tick.
					b.tunnel.Close()
					b.tunnel = nil
				}
			}
		}
	}
}

// tryUpgradeToDirect attempts to promote a proxy SSH tunnel to a direct one.
// It only runs if the current tunnel is indirect and at least 30s have passed
// since the last attempt. On success it swaps tunnels seamlessly; on failure
// the existing proxy tunnel keeps running.
func (b *Backend) tryUpgradeToDirect(ctx context.Context) {
	if b.tunnel == nil || b.tunnel.IsDirect() {
		return
	}
	if time.Since(b.lastUpgradeAttempt) < 30*time.Second {
		return
	}
	b.lastUpgradeAttempt = time.Now()

	inst := b.Instance
	if inst.PublicIPAddr == "" || inst.DirectSSHPort == 0 {
		return
	}

	// Try to create a direct-only tunnel (empty sshHost disables proxy fallback).
	factory := b.tunnelFactory
	if factory == nil {
		factory = NewSSHTunnel
	}
	directTunnel, err := factory(inst.PublicIPAddr, inst.DirectSSHPort, "", 0, b.keyPath, inst.ContainerPort)
	if err != nil {
		log.Printf("backend %d: direct SSH upgrade failed: %v", inst.ID, err)
		return
	}

	// Verify the new tunnel is healthy before swapping.
	tunnelURL := fmt.Sprintf("http://%s", directTunnel.LocalAddr())
	if err := b.httpHealthCheck(ctx, tunnelURL); err != nil {
		log.Printf("backend %d: direct SSH upgrade health check failed: %v", inst.ID, err)
		directTunnel.Close()
		return
	}

	// Swap: close old proxy tunnel, install new direct tunnel.
	old := b.tunnel
	b.tunnel = directTunnel
	b.baseURL = tunnelURL
	old.Close()
	log.Printf("backend %d: upgraded to direct SSH", inst.ID)
}

// ApplyLabel sets the managed label on the instance (best-effort).
// Called from main after initial health check succeeds.
func (b *Backend) ApplyLabel(ctx context.Context) {
	go b.setLabel(ctx, b.label)
}

// setLabel sets the instance label via the vast.ai API (best-effort, logged).
func (b *Backend) setLabel(ctx context.Context, label string) {
	if b.label == "" || b.vastClient == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := b.vastClient.SetLabel(ctx, b.Instance.ID, label); err != nil {
		log.Printf("backend %d: set label %q: %v", b.Instance.ID, label, err)
	} else {
		log.Printf("backend %d: label set to %q", b.Instance.ID, label)
		b.Instance.Label = label
	}
}

// clearLabelIfOurs clears the label only if it matches the managed label,
// to avoid overwriting labels set by the user externally.
func (b *Backend) clearLabelIfOurs(ctx context.Context) {
	if b.label == "" || b.vastClient == nil {
		return
	}
	if b.Instance.Label != b.label {
		return
	}
	b.setLabel(ctx, "")
}

// Close shuts down the backend and SSH tunnel.
func (b *Backend) Close() {
	// Best-effort: clear our label before tearing down.
	if b.label != "" && b.vastClient != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		b.clearLabelIfOurs(ctx)
		cancel()
	}
	b.healthy.Store(false)
	if b.tunnel != nil {
		b.tunnel.Close()
		b.tunnel = nil
	}
}

// AbortAll sends POST /abort_request with empty rid to abort all in-flight
// inference on this backend. SGLang stops generation for all active requests.
func (b *Backend) AbortAll(ctx context.Context) error {
	if b.baseURL == "" {
		return fmt.Errorf("no base URL")
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", b.baseURL+"/abort_request",
		strings.NewReader(`{"rid":""}`))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if b.Instance.JupyterToken != "" {
		req.Header.Set("Authorization", "Bearer "+b.Instance.JupyterToken)
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("abort returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// GPUUpdate is sent from a backend's health loop to the TUI.
type GPUUpdate struct {
	InstanceID int
	GPUs       []GPUMetric // per-GPU utilization and temperature
	IsDirect   bool        // true if SSH tunnel is direct (not proxied)
}
