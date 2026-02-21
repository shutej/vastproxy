# vastproxy — Agent Guidelines

## Formatting & Linting

Run `gofmt` and `go vet` before every commit:

```sh
gofmt -l .
go vet ./...
```

`gofmt -l .` lists files with incorrect formatting. If any are listed, fix them with `gofmt -w .`. All code must be properly formatted before committing.

`go vet ./...` reports suspicious constructs (e.g., unreachable code, incorrect format strings). Fix all warnings before committing.

## Testing

Run tests before committing:

```sh
go test ./...
```

### Race Detection

Periodically run the race detector to catch data races early:

```sh
go test -race ./...
```

This should be run:
- Before merging any PR that touches concurrent code (balancer, watcher, backend health loops)
- After modifying any code that uses goroutines, channels, sync.Mutex, or atomic operations
- As part of CI (add `-race` to the test step)

See https://go.dev/doc/articles/race_detector for details.

### Coverage

Check coverage with:

```sh
go test -coverprofile=cover.out ./backend/... ./proxy/... ./vast/...
go tool cover -func=cover.out
```

The `backend/ssh.go` functions have low coverage because they require real SSH connections. Everything else should stay above 80%.

## Architecture

- `vast/` — vast.ai API client, instance types, watcher (poller with fan-out)
- `backend/` — Backend struct (health checks, SSH tunnels, GPU metrics)
- `proxy/` — Round-robin balancer + `httputil.ReverseProxy` handler
- `tui/` — Bubbletea terminal UI
- `api/` — ogen-generated OpenAPI types (not used for HTTP handling)

## Key Design Decisions

- **Direct HTTP with Bearer auth** for all API traffic (no SSH tunnels for requests). The `jupyter_token` from the vast.ai instances API is the Bearer token for Caddy-proxied ports.
- **SSH is best-effort** — used only for GPU metrics via `nvidia-smi`. SSH failures don't affect request routing.
- **Round-robin load balancing** with an atomic counter. The balancer sorts backends by instance ID for stable ordering.
- **`httputil.ReverseProxy`** handles all request proxying, including SSE streaming (via `FlushInterval: -1`).
