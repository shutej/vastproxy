package vast

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const defaultAPIBase = "https://console.vast.ai/api/v0"

// Client is a vast.ai API client.
type Client struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

// NewClient creates a new vast.ai API client.
func NewClient(apiKey string) *Client {
	return &Client{
		apiKey:  apiKey,
		baseURL: defaultAPIBase,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// ListInstances fetches all instances from the vast.ai API.
func (c *Client) ListInstances(ctx context.Context) ([]Instance, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/instances/", nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("vast.ai API returned HTTP %d: %s", resp.StatusCode, body)
	}

	var result InstancesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return result.Instances, nil
}

// AttachSSHKey attaches a public SSH key to an instance.
func (c *Client) AttachSSHKey(ctx context.Context, instanceID int, publicKey string) error {
	body, _ := json.Marshal(map[string]string{"ssh_key": publicKey})
	url := fmt.Sprintf("%s/instances/%d/ssh/", c.baseURL, instanceID)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("attach ssh key returned HTTP %d: %s", resp.StatusCode, body)
	}
	return nil
}

// DestroyInstance destroys a vast.ai instance permanently.
// This is irreversible — all data on the instance is lost.
func (c *Client) DestroyInstance(ctx context.Context, instanceID int) error {
	url := fmt.Sprintf("%s/instances/%d/", c.baseURL, instanceID)
	req, err := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("destroy instance returned HTTP %d: %s", resp.StatusCode, body)
	}
	return nil
}
