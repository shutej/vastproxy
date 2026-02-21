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

	// Log to file since bubbletea captures stderr.
	logFile, err := os.OpenFile("vastproxy.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err == nil {
		log.SetOutput(logFile)
		defer logFile.Close()
	}

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

	proxyLabel := os.Getenv("VASTPROXY_LABEL")
	if proxyLabel == "" {
		proxyLabel = "proxied"
	}
	if proxyLabel == "none" {
		proxyLabel = ""
	}

	// Create vast.ai watcher.
	vastClient := vast.NewClient(apiKey)
	watcher := vast.NewWatcher(vastClient, 10*time.Second)

	// Create load balancer.
	balancer := proxy.NewBalancer()

	// Create sticky stats tracker (5-minute sliding window).
	stickyStats := proxy.NewStickyStats(5 * time.Minute)

	// Create reverse proxy handler.
	httpHandler := proxy.NewReverseProxy(balancer, stickyStats)

	// Create HTTP server.
	httpServer := &http.Server{
		Addr:    listenAddr,
		Handler: httpHandler,
	}

	// Channels for TUI communication.
	gpuCh := make(chan backend.GPUUpdate, 64)

	// Subscribe to watcher events — separate channels for TUI and backend manager.
	tuiEventCh := watcher.Subscribe()
	mgrEventCh := watcher.Subscribe()

	// Context for background goroutines.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle OS signals.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)

	// Start backend manager (reads from mgrEventCh).
	// Started before watcher so it's ready to receive events.
	go manageBackends(ctx, watcher, vastClient, mgrEventCh, balancer, gpuCh, keyPath, proxyLabel)

	// Start HTTP server.
	go func() {
		log.Printf("HTTP server listening on %s", listenAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP server error: %v", err)
		}
	}()

	// Create TUI model. Pass a start function that kicks off the watcher
	// once Init() runs, ensuring the TUI is ready to receive events.
	startWatcher := func() {
		go watcher.Start(ctx)
	}
	abortFn := func() {
		balancer.AbortAll(context.Background())
	}
	destroyFn := func() {
		watcher.DestroyAll(context.Background())
	}
	tuiModel := tui.NewModel(tuiEventCh, gpuCh, listenAddr, startWatcher, abortFn, destroyFn, stickyStats)
	p := tea.NewProgram(tuiModel, tea.WithAltScreen())

	go func() {
		<-sigCh
		cancel()
		p.Send(tea.Quit())
	}()

	// Run TUI (blocking). Init() will trigger the watcher start.
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
func manageBackends(ctx context.Context, watcher *vast.Watcher, vastClient *vast.Client, eventCh <-chan vast.InstanceEvent, bal *proxy.Balancer, gpuCh chan<- backend.GPUUpdate, keyPath string, proxyLabel string) {
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

		case evt, ok := <-eventCh:
			if !ok {
				return
			}

			switch evt.Type {
			case "added":
				inst := evt.Instance
				log.Printf("backend manager: adding instance %d (%s)", inst.ID, inst.DisplayName())
				be := backend.NewBackend(inst, keyPath, vastClient, proxyLabel)
				beCtx, beCancel := context.WithCancel(ctx)

				mu.Lock()
				backends[inst.ID] = be
				cancels[inst.ID] = beCancel
				mu.Unlock()

				updateBalancer()

				// Start health loop in background.
				go func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("backend %d: panic (recovered): %v", inst.ID, r)
							watcher.SetInstanceState(inst.ID, vast.StateUnhealthy)
						}
					}()
					watcher.SetInstanceState(inst.ID, vast.StateConnecting)

					be.EnsureSSH()
					if err := be.CheckHealth(beCtx); err != nil {
						log.Printf("backend %d: initial health check failed: %v", inst.ID, err)
						watcher.SetInstanceState(inst.ID, vast.StateUnhealthy)
					} else {
						log.Printf("backend %d: healthy", inst.ID)
						watcher.SetInstanceState(inst.ID, vast.StateHealthy)
						be.ApplyLabel(beCtx)
					}

					// Discover model name.
					if inst.ModelName == "" {
						if name, err := be.FetchModel(beCtx); err == nil {
							log.Printf("backend %d: model=%s", inst.ID, name)
							inst.ModelName = name
						}
					}

					// Continue with periodic health + GPU loop.
					be.StartHealthLoop(beCtx, watcher, gpuCh)
				}()

			case "removed":
				id := evt.Instance.ID
				log.Printf("backend manager: removing instance %d", id)
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
