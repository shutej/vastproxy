package vast

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const apiBase = "https://console.vast.ai/api/v0"

// Client is a vast.ai API client.
type Client struct {
	apiKey     string
	httpClient *http.Client
}

// NewClient creates a new vast.ai API client.
func NewClient(apiKey string) *Client {
	return &Client{
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// ListInstances fetches all instances from the vast.ai API.
func (c *Client) ListInstances(ctx context.Context) ([]Instance, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", apiBase+"/instances/", nil)
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
		return nil, fmt.Errorf("vast.ai API returned HTTP %d", resp.StatusCode)
	}

	var result InstancesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return result.Instances, nil
}
