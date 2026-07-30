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

// route is one API endpoint. Carrying the guard as a field makes the access
// check a declared property of the route instead of a wrapper each registration
// has to remember to type — and the zero value is the closed one.
type route struct {
	Pattern string
	Guard   guard
	Handler http.HandlerFunc
}

// apiRoutes lists every /api endpoint with the access check it requires.
//
// Convention the guard test relies on: an "{id}" wildcard is an instances.json
// session id, so such a route must be guarded per instance (or admin-only).
// Task ids use "{taskID}" — those handlers enforce the task ACL themselves via
// taskReadable, which also inherits the instance ACL.
func apiRoutes(env *config.EnvConfig, tm *tasks.Manager, cache *ec2.Cache, auth *AuthConfig) []route {
	return []route{
		{Pattern: "GET /api/instances", Guard: guardSignedIn, Handler: handleInstances(auth)},
		// Deliberately not admin-gated: any signed-in user may add an instance.
		{Pattern: "POST /api/instances", Guard: guardSignedIn, Handler: handleInstanceCreate(auth)},
		// guardInstance (the zero value): anyone who can see an instance may configure it.
		{Pattern: "PATCH /api/instances/{id}", Handler: handleInstanceUpdate(auth)},
		{Pattern: "GET /api/whoami", Guard: guardSignedIn, Handler: handleWhoami(auth)},
		{Pattern: "GET /api/users", Guard: guardSignedIn, Handler: handleUsers(auth)},
		{Pattern: "POST /api/users", Guard: guardAdmin, Handler: handleUserAdd(auth)},
		// {username}, not {id}: this is not an instance route, and it must stay
		// admin-only — guardInstance (the zero value) would be wrong here.
		{Pattern: "PATCH /api/users/{username}", Guard: guardAdmin, Handler: handleUserAdmin(auth)},
		{Pattern: "GET /api/statuses", Guard: guardSignedIn, Handler: handleStatuses(cache, auth)},
		{Pattern: "GET /api/config", Guard: guardSignedIn, Handler: handleConfig(env)},
		{Pattern: "GET /api/instance-types", Guard: guardSignedIn, Handler: handleInstanceTypes(env)},
		{Pattern: "GET /api/price", Guard: guardSignedIn, Handler: handlePrice(env)},
		{Pattern: "GET /api/azs", Guard: guardSignedIn, Handler: handleAZs(env)},

		// Long-running mutations — async via the task queue. Guard omitted on
		// purpose: the zero value is the per-instance reader ACL.
		{Pattern: "POST /api/start/{id}", Handler: handleStartSubmit(env, tm, cache)},
		{Pattern: "POST /api/stop/{id}", Handler: handleStopSubmit(env, tm, cache)},
		{Pattern: "POST /api/restart/{id}", Handler: handleRestartSubmit(env, tm, cache)},

		{Pattern: "GET /api/tasks", Guard: guardSignedIn, Handler: handleTaskList(tm, auth)},
		{Pattern: "GET /api/tasks/{taskID}", Guard: guardSignedIn, Handler: handleTaskGet(tm, auth)},
		{Pattern: "GET /api/tasks/{taskID}/stream", Guard: guardSignedIn, Handler: handleTaskStream(tm, auth)},
	}
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

	// Cost metering rides the same interval as the status poll: it reads the
	// snapshots that poll produces, so a finer tick would only re-read them.
	meter := newCostMeter(env, cache, interval, meterStatePath(statePath))
	go meter.Run(ctx)
	mux.HandleFunc("GET "+metricsPath, meter.handleMetrics())

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
	for _, rt := range apiRoutes(env, tm, cache, auth) {
		mux.HandleFunc(rt.Pattern, auth.wrap(rt.Guard, rt.Handler))
	}

	// Grafana behind this app's session, so an admin needs no second login.
	// Registered outside apiRoutes (no {id}, every method, and it is a proxy
	// rather than a handler), so the guard is applied here by hand — and it must
	// stay guardAdmin: the dashboards carry every user's spend.
	if upstream := grafanaUpstream(); upstream != "" {
		// Fails closed: with auth off, wrap() would hand the dashboards — every
		// user's spend — to anyone who can reach the port. Prod has had auth
		// silently disabled by a misnamed env var before.
		if auth == nil {
			fmt.Printf("ec2cp: NOT proxying %s (auth disabled); reach Grafana on %s directly\n", grafanaProxyPath, upstream)
		} else {
			h, err := handleGrafana(auth, upstream)
			if err != nil {
				return fmt.Errorf("grafana upstream %q: %w", upstream, err)
			}
			mux.HandleFunc(grafanaProxyPath, auth.wrap(guardAdmin, h))
			fmt.Printf("ec2cp: proxying %s to %s for admins\n", grafanaProxyPath, upstream)
		}
	}

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
