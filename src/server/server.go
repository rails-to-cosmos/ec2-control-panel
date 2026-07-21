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
	"strings"
	"time"

	"ec2cp/src/config"
	"ec2cp/src/ec2"
	"ec2cp/src/tasks"
)

//go:embed ui
var uiFS embed.FS

const (
	pollInterval = 30 * time.Second
	pollFanout   = 8
)

// Run starts the HTTP server. Blocks until ctx is cancelled or the server errors.
func Run(ctx context.Context, env *config.EnvConfig, port int) error {
	mux := http.NewServeMux()
	tm := tasks.NewManager(200)
	cache := ec2.NewCache(env, pollInterval, pollFanout)
	go cache.Run(ctx)

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
	mux.HandleFunc("POST /api/instances", handleInstanceCreate)
	mux.HandleFunc("GET /api/config", handleConfig(env))
	mux.HandleFunc("GET /api/instance-types", handleInstanceTypes(env))
	mux.HandleFunc("GET /api/instance-info", handleInstanceInfo(env))

	// Read-only — status is served from the cache; ?force=true bypasses it.
	mux.HandleFunc("GET /api/status/{id}", withStream(env, runStatusOp(cache)))
	mux.HandleFunc("GET /api/ip/{id}", withStream(env, runIPOp))
	mux.HandleFunc("POST /api/mount/{volume}/{id}", withStream(env, runMountOp))

	// Long-running mutations — async via task queue.
	mux.HandleFunc("POST /api/start/{id}", handleStartSubmit(env, tm, cache))
	mux.HandleFunc("POST /api/stop/{id}", handleStopSubmit(env, tm, cache))
	mux.HandleFunc("POST /api/restart/{id}", handleRestartSubmit(env, tm, cache))

	mux.HandleFunc("GET /api/tasks", handleTaskList(tm))
	mux.HandleFunc("GET /api/tasks/{id}", handleTaskGet(tm))
	mux.HandleFunc("GET /api/tasks/{id}/stream", handleTaskStream(tm))

	// Optional auth gate (GitLab OAuth and/or password). Disabled when no
	// method is configured, so local dev runs unauthenticated as before.
	var handler http.Handler = mux
	if auth := LoadAuthConfig(); auth != nil {
		auth.registerAuthRoutes(mux)
		handler = auth.middleware(mux)
		methods := []string{}
		if auth.oauthEnabled() {
			scope := "any GitLab user"
			if len(auth.oauth.AllowedUsers) > 0 {
				scope = fmt.Sprintf("%d allowed user(s)", len(auth.oauth.AllowedUsers))
			}
			methods = append(methods, fmt.Sprintf("GitLab OAuth (%s, %s)", auth.oauth.GitLabURL, scope))
		}
		if auth.passwordEnabled() {
			methods = append(methods, fmt.Sprintf("password (%d user(s))", len(auth.users)))
		}
		fmt.Printf("ec2cp: auth enabled — %s\n", strings.Join(methods, ", "))
	} else {
		fmt.Println("ec2cp: auth disabled (set GITLAB_URL/GITLAB_CLIENT_ID/... or EC2CP_USERS to enable)")
	}

	addr := fmt.Sprintf(":%d", port)
	fmt.Printf("ec2cp serve listening on %s\n", addr)
	srv := &http.Server{Addr: addr, Handler: handler}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
