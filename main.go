package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"
	"vastproxy/api"
	"vastproxy/backend"
	"vastproxy/proxy"
	"vastproxy/tui"
	"vastproxy/vast"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/joho/godotenv"
)

func main() {
	// Load .env (ignore error if missing).
	_ = godotenv.Load()

	apiKey := os.Getenv("VAST_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "VAST_API_KEY not set. Set it in .env or environment.")
		os.Exit(1)
	}

	keyPath := os.Getenv("SSH_KEY_PATH")
	if keyPath == "" {
		keyPath = "~/.ssh/id_rsa"
	}

	listenAddr := os.Getenv("LISTEN_ADDR")
	if listenAddr == "" {
		listenAddr = ":8080"
	}

	// Create vast.ai watcher.
	vastClient := vast.NewClient(apiKey)
	watcher := vast.NewWatcher(vastClient, 10*time.Second)

	// Create load balancer.
	balancer := proxy.NewBalancer()

	// Create ogen handler + streaming middleware.
	handler := proxy.NewHandler(balancer)
	ogenServer, err := api.NewServer(handler)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create API server: %v\n", err)
		os.Exit(1)
	}
	httpHandler := proxy.StreamingMiddleware(ogenServer, balancer)

	// Create HTTP server.
	httpServer := &http.Server{
		Addr:    listenAddr,
		Handler: httpHandler,
	}

	// Channels for TUI communication.
	gpuCh := make(chan backend.GPUUpdate, 64)

	// Create TUI model.
	tuiModel := tui.NewModel(watcher.Events(), gpuCh)
	p := tea.NewProgram(tuiModel, tea.WithAltScreen())

	// Context for background goroutines.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle OS signals.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		cancel()
		p.Send(tea.Quit())
	}()

	// Start watcher.
	go watcher.Start(ctx)

	// Start backend manager.
	go manageBackends(ctx, watcher, balancer, p, gpuCh, keyPath)

	// Start HTTP server.
	go func() {
		p.Send(tui.ServerStartedMsg{ListenAddr: listenAddr})
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			p.Send(tui.ErrorMsg{Error: err})
		}
	}()

	// Run TUI (blocking).
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
	}

	// Graceful shutdown.
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)
}

// manageBackends bridges watcher events to backend creation/removal.
func manageBackends(ctx context.Context, watcher *vast.Watcher, bal *proxy.Balancer, p *tea.Program, gpuCh chan<- backend.GPUUpdate, keyPath string) {
	backends := make(map[int]*backend.Backend)
	cancels := make(map[int]context.CancelFunc)
	var mu sync.Mutex

	updateBalancer := func() {
		mu.Lock()
		defer mu.Unlock()
		list := make([]*backend.Backend, 0, len(backends))
		for _, be := range backends {
			list = append(list, be)
		}
		bal.SetBackends(list)
	}

	for {
		select {
		case <-ctx.Done():
			mu.Lock()
			for id, be := range backends {
				be.Close()
				if cancelFn, ok := cancels[id]; ok {
					cancelFn()
				}
			}
			mu.Unlock()
			return

		case evt, ok := <-watcher.Events():
			if !ok {
				return
			}

			switch evt.Type {
			case "added":
				inst := evt.Instance
				be := backend.NewBackend(inst, keyPath)
				beCtx, beCancel := context.WithCancel(ctx)

				mu.Lock()
				backends[inst.ID] = be
				cancels[inst.ID] = beCancel
				mu.Unlock()

				updateBalancer()

				// Start health loop in background.
				go func() {
					// Initial health check.
					watcher.SetInstanceState(inst.ID, vast.StateConnecting)
					p.Send(tui.InstanceHealthChangedMsg{
						InstanceID: inst.ID,
						State:      vast.StateConnecting,
					})

					if err := be.CheckHealth(beCtx); err != nil {
						log.Printf("backend %d: initial health check failed: %v", inst.ID, err)
						watcher.SetInstanceState(inst.ID, vast.StateUnhealthy)
						p.Send(tui.InstanceHealthChangedMsg{
							InstanceID: inst.ID,
							State:      vast.StateUnhealthy,
						})
					} else {
						watcher.SetInstanceState(inst.ID, vast.StateHealthy)
						p.Send(tui.InstanceHealthChangedMsg{
							InstanceID: inst.ID,
							State:      vast.StateHealthy,
							ModelName:  inst.ModelName,
						})
					}

					// Discover model name.
					if inst.ModelName == "" {
						if name, err := be.FetchModel(beCtx); err == nil {
							inst.ModelName = name
							p.Send(tui.InstanceHealthChangedMsg{
								InstanceID: inst.ID,
								State:      inst.State,
								ModelName:  name,
							})
						}
					}

					// Continue with periodic health + GPU loop.
					be.StartHealthLoop(beCtx, watcher, gpuCh)
				}()

			case "removed":
				id := evt.Instance.ID
				mu.Lock()
				if be, ok := backends[id]; ok {
					be.Close()
					delete(backends, id)
				}
				if cancelFn, ok := cancels[id]; ok {
					cancelFn()
					delete(cancels, id)
				}
				mu.Unlock()

				updateBalancer()
			}
		}
	}
}
