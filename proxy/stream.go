package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// StreamingMiddleware wraps the ogen server handler to intercept streaming
// chat completion requests. ogen does not support text/event-stream responses,
// so we handle stream:true requests directly and relay SSE from the backend.
func StreamingMiddleware(ogenHandler http.Handler, balancer *Balancer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only intercept POST /v1/chat/completions.
		if r.Method != "POST" || (r.URL.Path != "/v1/chat/completions" && r.URL.Path != "/chat/completions") {
			ogenHandler.ServeHTTP(w, r)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, `{"error":{"message":"bad request"}}`, http.StatusBadRequest)
			return
		}

		if !isStreamingRequest(body) {
			// Non-streaming: restore body and pass to ogen.
			r.Body = io.NopCloser(bytes.NewReader(body))
			ogenHandler.ServeHTTP(w, r)
			return
		}

		handleStreamingRequest(w, r, body, balancer)
	})
}

func isStreamingRequest(body []byte) bool {
	var req struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &req)
	return req.Stream
}

func handleStreamingRequest(w http.ResponseWriter, r *http.Request, body []byte, balancer *Balancer) {
	be, err := balancer.Pick()
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, `{"error":{"message":"no backends available","type":"server_error"}}`)
		return
	}
	be.Acquire()
	defer be.Release()

	backendReq, err := http.NewRequestWithContext(r.Context(), "POST",
		be.BaseURL()+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		http.Error(w, `{"error":{"message":"internal error"}}`, http.StatusInternalServerError)
		return
	}
	backendReq.Header.Set("Content-Type", "application/json")
	if tok := be.Token(); tok != "" {
		backendReq.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := be.HTTPClient().Do(backendReq)
	if err != nil {
		http.Error(w, `{"error":{"message":"backend error"}}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Set SSE headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"error":{"message":"streaming not supported"}}`, http.StatusInternalServerError)
		return
	}

	// Relay SSE chunks line by line.
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Fprintf(w, "%s\n", line)
		if line == "" {
			// Empty line = SSE event boundary, flush.
			flusher.Flush()
		}
	}
	// Final flush.
	flusher.Flush()
}

func jsonReader(data []byte) io.Reader {
	return bytes.NewReader(data)
}
