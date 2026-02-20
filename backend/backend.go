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
	Instance   *vast.Instance
	baseURL    string
	httpClient *http.Client
	tunnel     *SSHTunnel
	activeReqs atomic.Int64
	healthy    atomic.Bool
	keyPath    string
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

// CheckHealth verifies connectivity to the backend. It first tries direct HTTP,
// then falls back to SSH tunnel.
func (b *Backend) CheckHealth(ctx context.Context) error {
	// Try direct HTTP first.
	if b.baseURL != "" {
		if err := b.httpHealthCheck(ctx, b.baseURL); err == nil {
			b.healthy.Store(true)
			return nil
		}
	}

	// Try via SSH tunnel if we have one.
	if b.tunnel != nil {
		tunnelURL := fmt.Sprintf("http://%s/v1", b.tunnel.LocalAddr())
		if err := b.httpHealthCheck(ctx, tunnelURL); err == nil {
			b.baseURL = tunnelURL
			b.healthy.Store(true)
			return nil
		}
	}

	// If no tunnel, try creating one.
	if b.tunnel == nil {
		tunnel, err := NewSSHTunnel(
			b.Instance.PublicIPAddr,
			b.Instance.DirectSSHPort,
			b.Instance.SSHHost,
			b.Instance.SSHPort,
			b.keyPath,
			b.Instance.ContainerPort,
		)
		if err != nil {
			b.healthy.Store(false)
			return fmt.Errorf("ssh tunnel: %w", err)
		}
		b.tunnel = tunnel

		// Give tunnel a moment to start.
		time.Sleep(500 * time.Millisecond)

		tunnelURL := fmt.Sprintf("http://%s/v1", tunnel.LocalAddr())
		if err := b.httpHealthCheck(ctx, tunnelURL); err == nil {
			b.baseURL = tunnelURL
			b.healthy.Store(true)
			return nil
		}
	}

	b.healthy.Store(false)
	return fmt.Errorf("all health checks failed for instance %d", b.Instance.ID)
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

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := b.CheckHealth(ctx); err != nil {
				log.Printf("backend %d: health check failed: %v", b.Instance.ID, err)

				// Check if instance is still in vast.ai
				if !watcher.HasInstance(b.Instance.ID) {
					log.Printf("backend %d: removed from vast.ai", b.Instance.ID)
					b.healthy.Store(false)
					return
				}

				// Try reconnecting SSH if it dropped
				if b.tunnel != nil {
					b.tunnel.Close()
					b.tunnel = nil
				}
				continue
			}

			// Discover model name if not yet known.
			if b.Instance.ModelName == "" {
				if name, err := b.FetchModel(ctx); err == nil {
					b.Instance.ModelName = name
				}
			}

			// Fetch GPU metrics via SSH.
			if b.tunnel != nil {
				if metrics, err := b.FetchGPUMetrics(); err == nil {
					select {
					case gpuCh <- GPUUpdate{
						InstanceID:  b.Instance.ID,
						Utilization: metrics.Utilization,
						Temperature: metrics.Temperature,
					}:
					default:
					}
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
