package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
	"time"
	"vastproxy/vast"
)

// Backend represents a single SGLang backend instance.
type Backend struct {
	Instance      *vast.Instance
	baseURL       string
	httpClient    *http.Client
	tunnel        *SSHTunnel
	activeReqs    atomic.Int64
	healthy       atomic.Bool
	keyPath       string
	sshFails      int       // consecutive SSH tunnel creation failures
	sshBackoffTil time.Time // don't retry SSH until this time
}

// NewBackend creates a backend for the given instance.
func NewBackend(inst *vast.Instance, keyPath string) *Backend {
	return &Backend{
		Instance: inst,
		baseURL:  inst.BaseURL,
		httpClient: &http.Client{
			Timeout: 5 * time.Minute, // Long timeout for streaming.
		},
		keyPath: keyPath,
	}
}

// BaseURL returns the URL to reach this backend's /v1 endpoint.
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

// CheckHealth verifies connectivity to the backend via direct HTTP.
func (b *Backend) CheckHealth(ctx context.Context) error {
	if b.baseURL == "" {
		b.healthy.Store(false)
		return fmt.Errorf("no base URL for instance %d", b.Instance.ID)
	}
	if err := b.httpHealthCheck(ctx, b.baseURL); err != nil {
		b.healthy.Store(false)
		return err
	}
	b.healthy.Store(true)
	return nil
}

// EnsureSSH establishes an SSH connection for GPU metrics (best-effort).
// Returns true if SSH is available.
func (b *Backend) EnsureSSH() bool {
	if b.tunnel != nil {
		return true
	}
	if time.Now().Before(b.sshBackoffTil) {
		return false
	}
	tunnel, err := NewSSHTunnel(
		b.Instance.PublicIPAddr,
		b.Instance.DirectSSHPort,
		b.Instance.SSHHost,
		b.Instance.SSHPort,
		b.keyPath,
		b.Instance.ContainerPort,
	)
	if err != nil {
		b.sshFails++
		// Exponential backoff: 10s, 20s, 40s, ... capped at 5m20s.
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

	req, err := http.NewRequestWithContext(ctx, "GET", baseURL+"/models", nil)
	if err != nil {
		return err
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
		"nvidia-smi --query-gpu=utilization.gpu,temperature.gpu --format=csv,noheader,nounits 2>/dev/null | head -1",
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

	req, err := http.NewRequestWithContext(ctx, "GET", b.baseURL+"/models", nil)
	if err != nil {
		return "", err
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
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	wasHealthy := b.healthy.Load()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := b.CheckHealth(ctx); err != nil {
				log.Printf("backend %d: health check failed: %v", b.Instance.ID, err)

				if wasHealthy {
					watcher.SetInstanceState(b.Instance.ID, vast.StateUnhealthy)
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
			}

			// Discover model name if not yet known.
			if b.Instance.ModelName == "" {
				if name, err := b.FetchModel(ctx); err == nil {
					log.Printf("backend %d: model=%s", b.Instance.ID, name)
					b.Instance.ModelName = name
				}
			}

			// Best-effort SSH for GPU metrics.
			if b.EnsureSSH() {
				if metrics, err := b.FetchGPUMetrics(); err == nil {
					select {
					case gpuCh <- GPUUpdate{
						InstanceID:  b.Instance.ID,
						Utilization: metrics.Utilization,
						Temperature: metrics.Temperature,
					}:
					default:
					}
				} else {
					// SSH session broke — tear down so EnsureSSH retries.
					b.tunnel.Close()
					b.tunnel = nil
				}
			}
		}
	}
}

// Close shuts down the backend and SSH tunnel.
func (b *Backend) Close() {
	b.healthy.Store(false)
	if b.tunnel != nil {
		b.tunnel.Close()
		b.tunnel = nil
	}
}

// GPUUpdate is sent from a backend's health loop to the TUI.
type GPUUpdate struct {
	InstanceID  int
	Utilization float64
	Temperature float64
}
