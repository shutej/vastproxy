package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"vastproxy/backend"
	"vastproxy/vast"
)

// fakeBackendServer creates an httptest.Server that echoes request info back.
func fakeBackendServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo back path + auth for test verification.
		auth := r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"path":%q,"auth":%q}`, r.URL.Path, auth)
	}))
}

func sseBackendServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := range 3 {
			fmt.Fprintf(w, "data: chunk %d\n\n", i)
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
}

func TestReverseProxyRouting(t *testing.T) {
	backendSrv := fakeBackendServer(t)
	defer backendSrv.Close()

	inst := &vast.Instance{ID: 1, JupyterToken: "secret"}
	be := backend.NewBackend(inst, "", nil, "")
	be.SetBaseURL(backendSrv.URL)
	be.SetHealthy(true)

	bal := NewBalancer()
	bal.SetBackends([]*backend.Backend{be})

	handler := NewReverseProxy(bal, nil)

	tests := []struct {
		name     string
		path     string
		wantPath string
	}{
		{"with /v1 prefix", "/v1/chat/completions", "/v1/chat/completions"},
		{"with /v1 prefix models", "/v1/models", "/v1/models"},
		{"without prefix", "/chat/completions", "/chat/completions"},
		{"bare /v1", "/v1", "/v1"},
		{"models with query", "/v1/models?foo=bar", "/v1/models"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}

			body := rec.Body.String()
			if !strings.Contains(body, tt.wantPath) {
				t.Errorf("body = %s, want path %q", body, tt.wantPath)
			}
		})
	}
}

func TestReverseProxyBearerAuth(t *testing.T) {
	backendSrv := fakeBackendServer(t)
	defer backendSrv.Close()

	inst := &vast.Instance{ID: 1, JupyterToken: "my-token"}
	be := backend.NewBackend(inst, "", nil, "")
	be.SetBaseURL(backendSrv.URL)
	be.SetHealthy(true)

	bal := NewBalancer()
	bal.SetBackends([]*backend.Backend{be})
	handler := NewReverseProxy(bal, nil)

	// Client sends its own auth — should be replaced with backend token.
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer client-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Bearer my-token") {
		t.Errorf("expected backend token, got: %s", body)
	}
	if strings.Contains(body, "client-token") {
		t.Error("client token should have been replaced")
	}
}

func TestReverseProxyNoBackends(t *testing.T) {
	bal := NewBalancer()
	handler := NewReverseProxy(bal, nil)

	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no backends available") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestReverseProxySSEStreaming(t *testing.T) {
	backendSrv := sseBackendServer(t)
	defer backendSrv.Close()

	inst := &vast.Instance{ID: 1, JupyterToken: "tok"}
	be := backend.NewBackend(inst, "", nil, "")
	be.SetBaseURL(backendSrv.URL)
	be.SetHealthy(true)

	bal := NewBalancer()
	bal.SetBackends([]*backend.Backend{be})
	handler := NewReverseProxy(bal, nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "chunk 0") || !strings.Contains(body, "[DONE]") {
		t.Errorf("SSE body missing expected chunks: %s", body)
	}
}

func TestReverseProxyAcquireRelease(t *testing.T) {
	backendSrv := fakeBackendServer(t)
	defer backendSrv.Close()

	inst := &vast.Instance{ID: 1}
	be := backend.NewBackend(inst, "", nil, "")
	be.SetBaseURL(backendSrv.URL)
	be.SetHealthy(true)

	bal := NewBalancer()
	bal.SetBackends([]*backend.Backend{be})
	handler := NewReverseProxy(bal, nil)

	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// After request completes, active count should be back to 0.
	if be.ActiveRequests() != 0 {
		t.Errorf("active requests = %d after request completed", be.ActiveRequests())
	}
}

func TestReverseProxyRoundRobinDistribution(t *testing.T) {
	// Create 3 backend HTTP servers, each echoing a unique instance ID via a
	// custom header and JSON body so we can verify exactly which backend was hit.
	type backendHit struct {
		serverURL string
		instID    int
	}
	servers := make([]*httptest.Server, 3)
	for i := range 3 {
		id := i + 1
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Backend-ID", fmt.Sprintf("%d", id))
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"backend":%d,"path":%q}`, id, r.URL.Path)
		}))
		defer servers[i].Close()
	}

	backends := make([]*backend.Backend, 3)
	for i, srv := range servers {
		inst := &vast.Instance{ID: i + 1}
		backends[i] = backend.NewBackend(inst, "", nil, "")
		backends[i].SetBaseURL(srv.URL)
		backends[i].SetHealthy(true)
	}

	bal := NewBalancer()
	bal.SetBackends(backends)
	handler := NewReverseProxy(bal, nil)

	// Send 30 requests and track which backend handled each.
	const totalReqs = 30
	hitCounts := map[string]int{} // X-Backend-ID -> count
	for range totalReqs {
		req := httptest.NewRequest("GET", "/v1/models", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}

		backendID := rec.Header().Get("X-Backend-ID")
		if backendID == "" {
			t.Fatal("missing X-Backend-ID header from backend response")
		}
		hitCounts[backendID]++
	}

	// All 3 backends must be hit.
	if len(hitCounts) != 3 {
		t.Fatalf("expected 3 distinct backends, got %d: %v", len(hitCounts), hitCounts)
	}

	// Each backend should be hit exactly totalReqs/3 times (perfect round-robin).
	for id, count := range hitCounts {
		if count != totalReqs/3 {
			t.Errorf("backend %s hit %d times, want %d", id, count, totalReqs/3)
		}
	}
}

func TestReverseProxyRoundRobinSkipsUnhealthy(t *testing.T) {
	// 3 servers, but only backends 1 and 3 are healthy.
	servers := make([]*httptest.Server, 3)
	for i := range 3 {
		id := i + 1
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Backend-ID", fmt.Sprintf("%d", id))
			fmt.Fprintf(w, `{"backend":%d}`, id)
		}))
		defer servers[i].Close()
	}

	backends := make([]*backend.Backend, 3)
	for i, srv := range servers {
		inst := &vast.Instance{ID: i + 1}
		backends[i] = backend.NewBackend(inst, "", nil, "")
		backends[i].SetBaseURL(srv.URL)
	}
	backends[0].SetHealthy(true)  // ID 1 — healthy
	backends[1].SetHealthy(false) // ID 2 — unhealthy
	backends[2].SetHealthy(true)  // ID 3 — healthy

	bal := NewBalancer()
	bal.SetBackends(backends)
	handler := NewReverseProxy(bal, nil)

	hitCounts := map[string]int{}
	for range 10 {
		req := httptest.NewRequest("GET", "/v1/models", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		backendID := rec.Header().Get("X-Backend-ID")
		hitCounts[backendID]++
	}

	// Only backends 1 and 3 should be hit, each 5 times.
	if hitCounts["2"] > 0 {
		t.Errorf("unhealthy backend 2 received %d requests", hitCounts["2"])
	}
	if len(hitCounts) != 2 {
		t.Fatalf("expected 2 distinct backends, got %d: %v", len(hitCounts), hitCounts)
	}
	for id, count := range hitCounts {
		if count != 5 {
			t.Errorf("backend %s hit %d times, want 5", id, count)
		}
	}
}

func TestReverseProxyConcurrentRoundRobin(t *testing.T) {
	// Verify round-robin works under concurrent load.
	servers := make([]*httptest.Server, 3)
	for i := range 3 {
		id := i + 1
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Backend-ID", fmt.Sprintf("%d", id))
			fmt.Fprintf(w, `{"backend":%d}`, id)
		}))
		defer servers[i].Close()
	}

	backends := make([]*backend.Backend, 3)
	for i, srv := range servers {
		inst := &vast.Instance{ID: i + 1}
		backends[i] = backend.NewBackend(inst, "", nil, "")
		backends[i].SetBaseURL(srv.URL)
		backends[i].SetHealthy(true)
	}

	bal := NewBalancer()
	bal.SetBackends(backends)
	handler := NewReverseProxy(bal, nil)

	const numWorkers = 10
	const reqsPerWorker = 9

	type result struct {
		backendID string
	}
	results := make(chan result, numWorkers*reqsPerWorker)

	var wg sync.WaitGroup
	for range numWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range reqsPerWorker {
				req := httptest.NewRequest("GET", "/v1/models", nil)
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				results <- result{backendID: rec.Header().Get("X-Backend-ID")}
			}
		}()
	}
	wg.Wait()
	close(results)

	hitCounts := map[string]int{}
	for r := range results {
		hitCounts[r.backendID]++
	}

	// All 3 backends must be hit.
	if len(hitCounts) != 3 {
		t.Fatalf("expected 3 distinct backends, got %d: %v", len(hitCounts), hitCounts)
	}

	// With round-robin and 90 total requests across 3 backends, each should get 30.
	totalReqs := numWorkers * reqsPerWorker
	for id, count := range hitCounts {
		if count != totalReqs/3 {
			t.Errorf("backend %s hit %d times, want %d", id, count, totalReqs/3)
		}
	}
}

func TestReverseProxyStickyHeaderInResponse(t *testing.T) {
	backendSrv := fakeBackendServer(t)
	defer backendSrv.Close()

	inst := &vast.Instance{ID: 42, JupyterToken: "tok"}
	be := backend.NewBackend(inst, "", nil, "")
	be.SetBaseURL(backendSrv.URL)
	be.SetHealthy(true)

	bal := NewBalancer()
	bal.SetBackends([]*backend.Backend{be})
	handler := NewReverseProxy(bal, nil)

	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got := rec.Header().Get(StickyHeader)
	if got != "42" {
		t.Errorf("response %s = %q, want %q", StickyHeader, got, "42")
	}
}

func TestReverseProxyStickyRouting(t *testing.T) {
	// Create 3 backends, each echoing their instance ID.
	servers := make([]*httptest.Server, 3)
	for i := range 3 {
		id := i + 1
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Backend-ID", fmt.Sprintf("%d", id))
			fmt.Fprintf(w, `{"backend":%d}`, id)
		}))
		defer servers[i].Close()
	}

	backends := make([]*backend.Backend, 3)
	for i, srv := range servers {
		inst := &vast.Instance{ID: i + 1}
		backends[i] = backend.NewBackend(inst, "", nil, "")
		backends[i].SetBaseURL(srv.URL)
		backends[i].SetHealthy(true)
	}

	bal := NewBalancer()
	bal.SetBackends(backends)
	handler := NewReverseProxy(bal, nil)

	// Send 10 requests all pinned to instance 2.
	for range 10 {
		req := httptest.NewRequest("GET", "/v1/models", nil)
		req.Header.Set(StickyHeader, "2")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		got := rec.Header().Get("X-Backend-ID")
		if got != "2" {
			t.Errorf("sticky request hit backend %s, want 2", got)
		}
		sticky := rec.Header().Get(StickyHeader)
		if sticky != "2" {
			t.Errorf("response %s = %q, want %q", StickyHeader, sticky, "2")
		}
	}
}

func TestReverseProxyStickyFallback(t *testing.T) {
	backendSrv := fakeBackendServer(t)
	defer backendSrv.Close()

	inst := &vast.Instance{ID: 1, JupyterToken: "tok"}
	be := backend.NewBackend(inst, "", nil, "")
	be.SetBaseURL(backendSrv.URL)
	be.SetHealthy(true)

	bal := NewBalancer()
	bal.SetBackends([]*backend.Backend{be})
	handler := NewReverseProxy(bal, nil)

	// Request a nonexistent instance — should fall back to round-robin.
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set(StickyHeader, "999")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fallback)", rec.Code)
	}
	// Should report the actual instance that served it.
	got := rec.Header().Get(StickyHeader)
	if got != "1" {
		t.Errorf("response %s = %q, want %q", StickyHeader, got, "1")
	}
}

func TestReverseProxyStickyHeaderNotForwarded(t *testing.T) {
	// Verify that X-VastProxy-Instance is stripped before reaching the backend.
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get(StickyHeader)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	inst := &vast.Instance{ID: 5}
	be := backend.NewBackend(inst, "", nil, "")
	be.SetBaseURL(srv.URL)
	be.SetHealthy(true)

	bal := NewBalancer()
	bal.SetBackends([]*backend.Backend{be})
	handler := NewReverseProxy(bal, nil)

	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set(StickyHeader, "5")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if gotHeader != "" {
		t.Errorf("backend received %s = %q, want empty (should be stripped)", StickyHeader, gotHeader)
	}
}

func TestReverseProxyBadBackendURL(t *testing.T) {
	// Backend with an unparseable URL should return 500.
	inst := &vast.Instance{ID: 1}
	be := backend.NewBackend(inst, "", nil, "")
	be.SetBaseURL("://bad-url")
	be.SetHealthy(true)

	bal := NewBalancer()
	bal.SetBackends([]*backend.Backend{be})
	handler := NewReverseProxy(bal, nil)

	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestReverseProxyBackendError(t *testing.T) {
	// Backend that immediately closes connection.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer srv.Close()

	inst := &vast.Instance{ID: 1}
	be := backend.NewBackend(inst, "", nil, "")
	be.SetBaseURL(srv.URL)
	be.SetHealthy(true)

	bal := NewBalancer()
	bal.SetBackends([]*backend.Backend{be})
	handler := NewReverseProxy(bal, nil)

	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}

	// Backend should be marked unhealthy immediately after the error.
	if be.IsHealthy() {
		t.Error("backend should be unhealthy after proxy error")
	}
}

func TestReverseProxyBackendErrorSkipsOnNextRequest(t *testing.T) {
	// One backend that always fails (closes connection), one that works.
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer badSrv.Close()

	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend-ID", "2")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer goodSrv.Close()

	badBe := backend.NewBackend(&vast.Instance{ID: 1}, "", nil, "")
	badBe.SetBaseURL(badSrv.URL)
	badBe.SetHealthy(true)

	goodBe := backend.NewBackend(&vast.Instance{ID: 2}, "", nil, "")
	goodBe.SetBaseURL(goodSrv.URL)
	goodBe.SetHealthy(true)

	bal := NewBalancer()
	bal.SetBackends([]*backend.Backend{badBe, goodBe})
	handler := NewReverseProxy(bal, nil)

	// First request hits the bad backend — should get 502 and mark it unhealthy.
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("first request: status = %d, want 502", rec.Code)
	}
	if badBe.IsHealthy() {
		t.Fatal("bad backend should be unhealthy after error")
	}

	// All subsequent requests should go to the good backend only.
	for i := range 5 {
		req = httptest.NewRequest("GET", "/v1/models", nil)
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("request %d: status = %d, want 200", i, rec.Code)
		}
		if got := rec.Header().Get("X-Backend-ID"); got != "2" {
			t.Errorf("request %d: routed to backend %s, want 2", i, got)
		}
	}
}

func TestReverseProxyForwardsRequestBody(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	inst := &vast.Instance{ID: 1}
	be := backend.NewBackend(inst, "", nil, "")
	be.SetBaseURL(srv.URL)
	be.SetHealthy(true)

	bal := NewBalancer()
	bal.SetBackends([]*backend.Backend{be})
	handler := NewReverseProxy(bal, nil)

	body := `{"model":"test","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if gotBody != body {
		t.Errorf("backend got body %q, want %q", gotBody, body)
	}
}
