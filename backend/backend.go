package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
	"vastproxy/vast"
)

// Backend represents a single SGLang backend instance.
// All HTTP traffic is routed through an SSH tunnel — no direct HTTP to instances.
type Backend struct {
	Instance      *vast.Instance
	baseURL       string // tunnel URL set by CheckHealth (e.g. "http://127.0.0.1:PORT/v1")
	httpClient    *http.Client
	tunnel        Tunnel
	tunnelFactory TunnelFactory // creates tunnels; nil = use NewSSHTunnel
	activeReqs    atomic.Int64
	healthy       atomic.Bool
	keyPath       string
	vastClient    *vast.Client
	sshFails      int       // consecutive SSH tunnel creation failures
	sshBackoffTil time.Time // don't retry SSH until this time
	sshKeyPushed  bool      // whether we've attempted to attach our SSH key
}

// NewBackend creates a backend for the given instance.
func NewBackend(inst *vast.Instance, keyPath string, vastClient *vast.Client) *Backend {
	return &Backend{
		Instance: inst,
		httpClient: &http.Client{
			Timeout: 5 * time.Minute,
		},
		keyPath:    keyPath,
		vastClient: vastClient,
	}
}

// Token returns the instance's jupyter_token for Bearer auth on proxied ports.
func (b *Backend) Token() string {
	return b.Instance.JupyterToken
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

// CheckHealth verifies connectivity to the backend via the SSH tunnel.
// All HTTP traffic goes through the tunnel — no direct HTTP to instances.
func (b *Backend) CheckHealth(ctx context.Context) error {
	if b.tunnel == nil {
		b.healthy.Store(false)
		return fmt.Errorf("no tunnel for instance %d", b.Instance.ID)
	}

	tunnelURL := fmt.Sprintf("http://%s/v1", b.tunnel.LocalAddr())
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

		// NOTE: SSH key push via vast.ai API is disabled — it was returning
		// 401 and is not needed for request routing (SSH is metrics-only).
		// To re-enable, uncomment the pushSSHKey block below.
		//
		// errStr := err.Error()
		// if !b.sshKeyPushed && b.vastClient != nil &&
		// 	(strings.Contains(errStr, "unable to authenticate") ||
		// 		strings.Contains(errStr, "handshake failed")) {
		// 	b.pushSSHKey()
		// }

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

// pushSSHKey reads the public key and attaches it to the instance via the API.
func (b *Backend) pushSSHKey() {
	b.sshKeyPushed = true

	keyPath := b.keyPath
	if keyPath != "" && keyPath[0] == '~' {
		if home, err := os.UserHomeDir(); err == nil {
			keyPath = filepath.Join(home, keyPath[1:])
		}
	}

	// Try .pub extension first, then the key path itself (in case it IS the pub key).
	pubPath := keyPath + ".pub"
	data, err := os.ReadFile(pubPath)
	if err != nil {
		data, err = os.ReadFile(keyPath)
		if err != nil {
			log.Printf("backend %d: cannot read ssh public key: %v", b.Instance.ID, err)
			return
		}
	}

	pubKey := strings.TrimSpace(string(data))
	if !strings.HasPrefix(pubKey, "ssh-") && !strings.HasPrefix(pubKey, "ecdsa-") {
		log.Printf("backend %d: %s doesn't look like a public key, skipping", b.Instance.ID, pubPath)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := b.vastClient.AttachSSHKey(ctx, b.Instance.ID, pubKey); err != nil {
		log.Printf("backend %d: attach ssh key failed: %v", b.Instance.ID, err)
	} else {
		log.Printf("backend %d: attached ssh public key via API", b.Instance.ID)
	}
}

func (b *Backend) httpHealthCheck(ctx context.Context, baseURL string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", baseURL+"/models", nil)
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

	req, err := http.NewRequestWithContext(ctx, "GET", b.baseURL+"/models", nil)
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
	ticker := time.NewTicker(5 * time.Second)
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

			// Fetch GPU metrics via SSH.
			if b.tunnel != nil {
				if metrics, err := b.FetchGPUMetrics(); err == nil {
					select {
					case gpuCh <- GPUUpdate{
						InstanceID: b.Instance.ID,
						GPUs:       metrics.GPUs,
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

// Close shuts down the backend and SSH tunnel.
func (b *Backend) Close() {
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

	// The /abort_request endpoint is at the server root, not under /v1.
	// Strip /v1 suffix from baseURL to get the server root.
	serverURL := strings.TrimSuffix(b.baseURL, "/v1")

	req, err := http.NewRequestWithContext(ctx, "POST", serverURL+"/abort_request",
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
}
