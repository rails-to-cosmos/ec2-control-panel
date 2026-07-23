// Package server hosts the HTTP API + embedded vanilla-JS UI. It uses the same
// business logic the CLI does (src/ec2), differing only in how it drives
// progress output (task buffers streamed to the browser).
package server

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"ec2cp/src/config"
	"ec2cp/src/ec2"
	"ec2cp/src/tasks"
)

//go:embed ui
var uiFS embed.FS

const (
	// Status polling. The fanout is what bounds a full sweep: each instance
	// costs a handful of AWS round-trips, so more concurrency shortens the
	// cycle far more than a shorter interval does.
	defaultPollInterval = 15 * time.Second
	defaultPollFanout   = 16
	defaultStateFile    = "state/status-cache.json"
)

// pollSettings resolves the poll tunables, allowing EC2CP_POLL_INTERVAL
// (seconds), EC2CP_POLL_FANOUT and EC2CP_STATE_FILE to override the defaults.
func pollSettings() (time.Duration, int, string) {
	interval := defaultPollInterval
	if v, err := strconv.Atoi(os.Getenv("EC2CP_POLL_INTERVAL")); err == nil && v > 0 {
		interval = time.Duration(v) * time.Second
	}
	fanout := defaultPollFanout
	if v, err := strconv.Atoi(os.Getenv("EC2CP_POLL_FANOUT")); err == nil && v > 0 {
		fanout = v
	}
	state := defaultStateFile
	if v := os.Getenv("EC2CP_STATE_FILE"); v != "" {
		state = v
	}
	return interval, fanout, state
}

// warmCaches pre-populates the instance-type lists (one AWS round-trip per AZ,
// slow on a cold cache) and the approximate spot prices for each instance's
// configured type, so the UI table renders with its dropdowns and prices
// already available instead of blocking on AWS at first paint.
func warmCaches(ctx context.Context, env *config.EnvConfig) {
	insts, err := config.LoadInstances()
	if err != nil {
		return
	}
	azs := map[string]bool{env.AvailabilityZone: true}
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for _, cfg := range insts {
		az := ec2.FirstNonEmpty(cfg.AvailabilityZone, env.AvailabilityZone)
		instType := ec2.FirstNonEmpty(cfg.InstanceType, env.DefaultInstanceType)
		azs[az] = true
		if key := instType + "|" + az; instType != "" && az != "" && !seen[key] {
			seen[key] = true
			wg.Add(1)
			go func(t, a string) { defer wg.Done(); _, _ = pricesFor(ctx, env, t, a) }(instType, az)
		}
	}
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = availabilityZones(ctx, env) }()
	for az := range azs {
		if az == "" {
			continue
		}
		wg.Add(1)
		go func(a string) { defer wg.Done(); _, _ = instanceTypesForAZ(ctx, env, a) }(az)
	}
	wg.Wait()
}

// Run starts the HTTP server. Blocks until ctx is cancelled or the server errors.
func Run(ctx context.Context, env *config.EnvConfig, port int) error {
	mux := http.NewServeMux()
	tm := tasks.NewManager(200)
	interval, fanout, statePath := pollSettings()
	cache := ec2.NewCache(env, interval, fanout, statePath)
	fmt.Printf("ec2cp: status poll every %s, fanout %d, state %s\n", interval, fanout, statePath)
	go cache.Run(ctx)
	go warmCaches(ctx, env)

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		page, err := uiFS.ReadFile("ui/index.html")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// The UI ships inside the binary, so a redeploy changes it — don't let a
		// browser serve a stale copy against the new API.
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(page)
	})

	// Static assets (Pico CSS, etc.) served from the embedded ui/ directory.
	assetsFS, err := fs.Sub(uiFS, "ui")
	if err != nil {
		return fmt.Errorf("embed sub: %w", err)
	}
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(assetsFS))))

	auth := LoadAuthConfig()
	// protect wraps a per-instance handler with the reader ACL (no-op when
	// auth is disabled).
	protect := auth.RequireInstanceAccess

	mux.HandleFunc("GET /api/instances", handleInstances(auth))
	mux.HandleFunc("POST /api/instances", handleInstanceCreate(auth))
	mux.HandleFunc("PATCH /api/instances/{id}", auth.requireAdmin(handleInstanceUpdate(auth)))
	mux.HandleFunc("GET /api/whoami", handleWhoami(auth))
	mux.HandleFunc("GET /api/users", handleUsers(auth))
	mux.HandleFunc("POST /api/users", auth.requireAdmin(handleUserAdd(auth)))
	mux.HandleFunc("GET /api/statuses", handleStatuses(cache, auth))
	mux.HandleFunc("GET /api/config", handleConfig(env))
	mux.HandleFunc("GET /api/instance-types", handleInstanceTypes(env))
	mux.HandleFunc("GET /api/price", handlePrice(env))
	mux.HandleFunc("GET /api/azs", handleAZs(env))

	// Long-running mutations — async via task queue.
	mux.HandleFunc("POST /api/start/{id}", protect(handleStartSubmit(env, tm, cache)))
	mux.HandleFunc("POST /api/stop/{id}", protect(handleStopSubmit(env, tm, cache)))
	mux.HandleFunc("POST /api/restart/{id}", protect(handleRestartSubmit(env, tm, cache)))

	mux.HandleFunc("GET /api/tasks", handleTaskList(tm, auth))
	mux.HandleFunc("GET /api/tasks/{id}", handleTaskGet(tm, auth))
	mux.HandleFunc("GET /api/tasks/{id}/stream", handleTaskStream(tm, auth))

	// Optional auth gate (Google OAuth and/or password). Disabled when no
	// method is configured, so local dev runs unauthenticated as before.
	var handler http.Handler = mux
	if auth != nil {
		auth.registerAuthRoutes(mux)
		handler = auth.middleware(mux)
		methods := []string{}
		if auth.oauthEnabled() {
			scope := "any Google account"
			if auth.oauth.AllowedDomain != "" {
				scope = "domain " + auth.oauth.AllowedDomain
			}
			if len(auth.oauth.AllowedUsers) > 0 {
				scope = fmt.Sprintf("%d allowed user(s)", len(auth.oauth.AllowedUsers))
			}
			methods = append(methods, fmt.Sprintf("Google OAuth (%s)", scope))
		}
		if auth.passwordEnabled() {
			methods = append(methods, fmt.Sprintf("password (%d user(s))", len(auth.users)))
		}
		fmt.Printf("ec2cp: auth enabled — %s\n", strings.Join(methods, ", "))
	} else {
		fmt.Println("ec2cp: auth disabled (set GOOGLE_CLIENT_ID/GOOGLE_CLIENT_SECRET/OAUTH_CALLBACK_URL or EC2CP_USERS to enable)")
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
