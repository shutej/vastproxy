package proxy

import (
	"context"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// statusRecorder wraps http.ResponseWriter to capture the status code and
// count bytes written, for request logging.
type statusRecorder struct {
	http.ResponseWriter
	status       int
	bytesWritten int64
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Write(b []byte) (int, error) {
	n, err := sr.ResponseWriter.Write(b)
	sr.bytesWritten += int64(n)
	return n, err
}

// Flush implements http.Flusher for streaming (SSE) support.
func (sr *statusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// NewReverseProxy creates an http.Handler that load-balances all incoming
// requests across healthy backends using the balancer's round-robin selection.
//
// Incoming path is forwarded as-is to the backend's /v1 endpoint. For example,
// a request to /v1/chat/completions is proxied to <backend>/v1/chat/completions.
// Requests without the /v1 prefix get it prepended.
func NewReverseProxy(balancer *Balancer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		be, err := balancer.Pick()
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":{"message":"no backends available","type":"server_error"}}`))
			return
		}
		be.Acquire()
		balancer.Acquire()
		defer func() {
			be.Release()
			if remaining := balancer.Release(); remaining == 0 {
				// Last client disconnected — abort all in-flight inference
				// on backends to free GPU resources.
				log.Printf("proxy: last request finished, aborting all backend work")
				go balancer.AbortAll(context.Background())
			}
		}()

		target, err := url.Parse(be.BaseURL())
		if err != nil {
			log.Printf("proxy: bad backend URL %q: %v", be.BaseURL(), err)
			http.Error(w, `{"error":{"message":"internal error"}}`, http.StatusInternalServerError)
			return
		}

		// The backend's BaseURL already ends with /v1.
		// If the incoming path starts with /v1/, strip it so we don't double up.
		inPath := r.URL.Path
		if after, ok := strings.CutPrefix(inPath, "/v1"); ok {
			if after == "" {
				inPath = "/"
			} else {
				inPath = after // already starts with /
			}
		}

		// Capture the upstream status code from the backend response.
		var upstreamStatus atomic.Int32

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		proxy := &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				req.URL.Scheme = target.Scheme
				req.URL.Host = target.Host
				req.URL.Path = target.Path + inPath // target.Path = /v1, inPath = /chat/completions
				req.URL.RawQuery = r.URL.RawQuery
				req.Host = target.Host

				// Replace any client auth with the backend's bearer token.
				req.Header.Del("Authorization")
				if tok := be.Token(); tok != "" {
					req.Header.Set("Authorization", "Bearer "+tok)
				}
			},
			ModifyResponse: func(resp *http.Response) error {
				upstreamStatus.Store(int32(resp.StatusCode))
				return nil
			},
			Transport: be.HTTPClient().Transport,
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				log.Printf("proxy: backend %d error: %v", be.Instance.ID, err)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadGateway)
				w.Write([]byte(`{"error":{"message":"backend error","type":"server_error"}}`))
			},
			// Streaming (SSE) works automatically — ReverseProxy flushes
			// the response when the backend sends data, because Go's
			// default FlushInterval is -1 for responses without Content-Length.
			FlushInterval: -1,
		}

		proxy.ServeHTTP(rec, r)

		elapsed := time.Since(start)
		us := upstreamStatus.Load()
		log.Printf("proxy: %s %s → backend %d upstream=%d status=%d bytes=%d duration=%s",
			r.Method, r.URL.Path, be.Instance.ID, us, rec.status, rec.bytesWritten, elapsed.Round(time.Millisecond))
	})
}
