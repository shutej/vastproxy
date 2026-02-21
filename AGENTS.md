# vastproxy — Agent Guidelines

## Formatting & Linting

Run `gofmt` and `go vet` before every commit:

```sh
gofmt -w -s .
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

- **SSH is compulsory** — all HTTP traffic is routed through SSH tunnels. There is no direct HTTP to instances. SSH also provides the channel for GPU metrics via `nvidia-smi`. If the tunnel is down, the backend is marked unhealthy and receives no traffic.
- **Proxy SSH first, direct SSH as fallback.** `NewSSHTunnel` tries the vast.ai proxy endpoint (`sshHost:sshPort`) first because it is more reliable, then falls back to direct SSH (`publicIP:directSSHPort`). The `Tunnel.IsDirect()` method tracks which path was used.
- **Bearer auth is still used** on the tunneled connection. The `jupyter_token` from the vast.ai instances API is sent as `Authorization: Bearer <token>` on health checks, model queries, abort requests, and proxied client requests. Caddy inside the container still expects it.
- **Sticky routing** via the `X-VastProxy-Instance` header. The proxy sets it on every response; clients can send it on subsequent requests to pin to a specific backend for KV cache locality (best-effort — falls back to round-robin).
- **Round-robin load balancing** with an atomic counter. The balancer sorts backends by instance ID for stable ordering.
- **`httputil.ReverseProxy`** handles all request proxying, including SSE streaming (via `FlushInterval: -1`).
