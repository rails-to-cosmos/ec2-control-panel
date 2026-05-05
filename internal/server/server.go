// Package server hosts the HTTP API + embedded vanilla-JS UI. It uses the
// same business logic the CLI does (internal/ec2), differing only in how it
// drives progress output (per-request streams or task buffers).
package server

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"net/http"

	"ec2cp/internal/config"
	"ec2cp/internal/tasks"
)

//go:embed ui
var uiFS embed.FS

// Run starts the HTTP server. Blocks until ctx is cancelled or the server errors.
func Run(ctx context.Context, env *config.EnvConfig, port int) error {
	mux := http.NewServeMux()
	tm := tasks.NewManager(200)

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		page, err := uiFS.ReadFile("ui/index.html")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	})

	// Static assets (Pico CSS, etc.) served from the embedded ui/ directory.
	assetsFS, err := fs.Sub(uiFS, "ui")
	if err != nil {
		return fmt.Errorf("embed sub: %w", err)
	}
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(assetsFS))))

	mux.HandleFunc("GET /api/instances", handleInstances)
	mux.HandleFunc("GET /api/config", handleConfig(env))
	mux.HandleFunc("GET /api/instance-types", handleInstanceTypes(env))

	// Read-only / fast — stay synchronous, stream the response inline.
	mux.HandleFunc("GET /api/status/{id}", withStream(env, runStatusOp))
	mux.HandleFunc("GET /api/ip/{id}", withStream(env, runIPOp))
	mux.HandleFunc("POST /api/mount/{volume}/{id}", withStream(env, runMountOp))

	// Long-running mutations — async via task queue.
	mux.HandleFunc("POST /api/start/{id}", handleStartSubmit(env, tm))
	mux.HandleFunc("POST /api/stop/{id}", handleStopSubmit(env, tm))
	mux.HandleFunc("POST /api/restart/{id}", handleRestartSubmit(env, tm))

	mux.HandleFunc("GET /api/tasks", handleTaskList(tm))
	mux.HandleFunc("GET /api/tasks/{id}", handleTaskGet(tm))
	mux.HandleFunc("GET /api/tasks/{id}/stream", handleTaskStream(tm))

	addr := fmt.Sprintf(":%d", port)
	fmt.Printf("ec2cp serve listening on %s\n", addr)
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
