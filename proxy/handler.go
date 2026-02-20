package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"vastproxy/api"
)

// Handler implements the ogen-generated api.Handler interface.
type Handler struct {
	api.UnimplementedHandler
	balancer *Balancer
}

// NewHandler creates a new proxy handler.
func NewHandler(balancer *Balancer) *Handler {
	return &Handler{
		balancer: balancer,
	}
}

// CreateChatCompletion proxies a non-streaming chat completion to a backend.
func (h *Handler) CreateChatCompletion(ctx context.Context, req *api.CreateChatCompletionRequest) (*api.CreateChatCompletionResponse, error) {
	be, err := h.balancer.Pick()
	if err != nil {
		return nil, err
	}
	be.Acquire()
	defer be.Release()

	// Marshal the request and forward via raw HTTP to the backend.
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	backendReq, err := http.NewRequestWithContext(ctx, "POST",
		be.BaseURL()+"/chat/completions",
		jsonReader(body))
	if err != nil {
		return nil, fmt.Errorf("create backend request: %w", err)
	}
	backendReq.Header.Set("Content-Type", "application/json")
	if tok := be.Token(); tok != "" {
		backendReq.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := be.HTTPClient().Do(backendReq)
	if err != nil {
		return nil, fmt.Errorf("backend request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("backend returned %d", resp.StatusCode)
	}

	var result api.CreateChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode backend response: %w", err)
	}
	return &result, nil
}

// ListModels proxies a list models request to a backend.
func (h *Handler) ListModels(ctx context.Context) (*api.ListModelsResponse, error) {
	be, err := h.balancer.Pick()
	if err != nil {
		return nil, err
	}

	backendReq, err := http.NewRequestWithContext(ctx, "GET", be.BaseURL()+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("create backend request: %w", err)
	}
	if tok := be.Token(); tok != "" {
		backendReq.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := be.HTTPClient().Do(backendReq)
	if err != nil {
		return nil, fmt.Errorf("backend request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("backend returned %d", resp.StatusCode)
	}

	var result api.ListModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode backend response: %w", err)
	}
	return &result, nil
}

// RetrieveModel proxies a retrieve model request to a backend.
func (h *Handler) RetrieveModel(ctx context.Context, params api.RetrieveModelParams) (*api.Model, error) {
	be, err := h.balancer.Pick()
	if err != nil {
		return nil, err
	}

	backendReq, err := http.NewRequestWithContext(ctx, "GET",
		be.BaseURL()+"/models/"+params.Model, nil)
	if err != nil {
		return nil, fmt.Errorf("create backend request: %w", err)
	}
	if tok := be.Token(); tok != "" {
		backendReq.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := be.HTTPClient().Do(backendReq)
	if err != nil {
		return nil, fmt.Errorf("backend request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("backend returned %d", resp.StatusCode)
	}

	var result api.Model
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode backend response: %w", err)
	}
	return &result, nil
}
