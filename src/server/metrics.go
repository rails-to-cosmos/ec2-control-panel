package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ec2cp/src/config"
	"ec2cp/src/ec2"
)

// metricsPath is public in the auth middleware: Prometheus cannot hold a
// session cookie. EC2CP_METRICS_TOKEN gates it instead when set.
const metricsPath = "/metrics"

// meterAccount is one instance's running total. Counters are cumulative for the
// life of the meter file, so Prometheus sees them rise monotonically across
// deploys instead of resetting on every container recreation.
type meterAccount struct {
	Owner    string  `json:"owner"`
	Type     string  `json:"type"`
	Model    string  `json:"model"` // "spot" | "ondemand"
	AZ       string  `json:"az"`
	Running  bool    `json:"running"`
	Rate     float64 `json:"rate"`     // last known $/hour, 0 when not running
	CostUSD  float64 `json:"costUsd"`  // accrued since the meter file was created
	Seconds  float64 `json:"seconds"`  // running seconds, same window
	Sessions int64   `json:"sessions"` // stopped -> running transitions
}

// costMeter samples the status cache on a ticker and turns "this instance was
// running at this rate for this long" into Prometheus counters. It reads the
// same memoized price lookups the UI uses, so metering costs no extra AWS
// calls beyond the first lookup per (type, AZ).
type costMeter struct {
	env      *config.EnvConfig
	cache    snapshotSource
	interval time.Duration
	// rate resolves $/hour. A field so the accounting can be tested without
	// reaching AWS; nil means the real pricing lookup.
	rate func(ctx context.Context, instType, az, model string) float64
	// statePath persists the counters. Empty disables persistence, which is
	// only correct for tests — in production a deploy would zero every total.
	statePath string

	mu       sync.RWMutex
	accounts map[string]*meterAccount
	lastTick time.Time
}

// snapshotSource is the slice of the status cache the meter needs.
type snapshotSource interface {
	Get(sessionID string) *ec2.Snapshot
}

func newCostMeter(env *config.EnvConfig, cache snapshotSource, interval time.Duration, statePath string) *costMeter {
	return &costMeter{
		env:       env,
		cache:     cache,
		interval:  interval,
		statePath: statePath,
		accounts:  map[string]*meterAccount{},
	}
}

// meterStatePath puts the meter file beside the status cache, which already has
// to live on a mounted directory for the atomic write to work.
func meterStatePath(stateFile string) string {
	if stateFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(stateFile), "cost-meter.json")
}

func (m *costMeter) load() {
	if m.statePath == "" {
		return
	}
	data, err := os.ReadFile(m.statePath)
	if err != nil {
		return
	}
	var accounts map[string]*meterAccount
	if err := json.Unmarshal(data, &accounts); err != nil {
		log.Printf("metrics: ignoring unreadable meter file %s: %v", m.statePath, err)
		return
	}
	m.mu.Lock()
	for k, v := range accounts {
		if v != nil {
			// Whether it is still running is the next tick's business; only the
			// totals carry over.
			v.Running = false
			v.Rate = 0
			m.accounts[k] = v
		}
	}
	n := len(m.accounts)
	m.mu.Unlock()
	log.Printf("metrics: restored %d cost accounts from %s", n, m.statePath)
}

func (m *costMeter) save() {
	if m.statePath == "" {
		return
	}
	m.mu.RLock()
	data, err := json.Marshal(m.accounts)
	m.mu.RUnlock()
	if err != nil {
		return
	}
	if err := config.WriteFileAtomic(m.statePath, data); err != nil {
		log.Printf("metrics: persist %s: %v", m.statePath, err)
	}
}

func (m *costMeter) Run(ctx context.Context) {
	m.load()
	m.tick(ctx)

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			m.save()
			return
		case <-ticker.C:
			m.tick(ctx)
			m.save()
		}
	}
}

// tick charges every running instance for the time since the previous tick.
// The first tick charges nothing: it only establishes the baseline.
func (m *costMeter) tick(ctx context.Context) {
	now := time.Now()

	m.mu.Lock()
	elapsed := 0.0
	if !m.lastTick.IsZero() {
		elapsed = now.Sub(m.lastTick).Seconds()
	}
	m.lastTick = now
	m.mu.Unlock()

	// A pause longer than a few intervals means the process was down, not that
	// the instance ran unobserved — don't bill the gap.
	if max := 4 * m.interval.Seconds(); elapsed > max {
		elapsed = 0
	}

	insts, err := config.LoadInstances()
	if err != nil {
		log.Printf("metrics: load instances: %v", err)
		return
	}

	for id, cfg := range insts {
		snap := m.cache.Get(id)
		running := snap != nil && snap.Instance != nil && snap.Instance.State == "running"

		acct := m.account(id)
		acct.Owner = cfg.Owner
		if running {
			acct.Type = snap.Instance.InstanceType
			acct.Model = purchaseModel(snap.Instance.Lifecycle)
			acct.AZ = snap.AZ
		}

		rate := 0.0
		if running {
			lookup := m.rate
			if lookup == nil {
				lookup = m.rateFor
			}
			rate = lookup(ctx, acct.Type, acct.AZ, acct.Model)
		}

		m.mu.Lock()
		if running && !acct.Running {
			acct.Sessions++
		}
		if running {
			acct.Seconds += elapsed
			acct.CostUSD += rate * elapsed / 3600
		}
		acct.Running = running
		acct.Rate = rate
		m.mu.Unlock()
	}

	// Forget instances that are gone from instances.json, the way the status
	// cache does — otherwise their series linger forever.
	m.mu.Lock()
	for id := range m.accounts {
		if _, ok := insts[id]; !ok {
			delete(m.accounts, id)
		}
	}
	m.mu.Unlock()
}

func (m *costMeter) account(id string) *meterAccount {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a, ok := m.accounts[id]; ok {
		return a
	}
	a := &meterAccount{}
	m.accounts[id] = a
	return a
}

// rateFor resolves the hourly price for the purchase model actually in use,
// falling back to the other model when AWS reports no price for it.
func (m *costMeter) rateFor(ctx context.Context, instType, az, model string) float64 {
	if instType == "" || az == "" {
		return 0
	}
	prices, err := pricesFor(ctx, m.env, instType, az)
	if err != nil {
		return 0
	}
	primary, fallback := "spot", "onDemand"
	if model != "spot" {
		primary, fallback = "onDemand", "spot"
	}
	if v := parsePrice(prices[primary]); v > 0 {
		return v
	}
	return parsePrice(prices[fallback])
}

func parsePrice(v any) float64 {
	s, _ := v.(string)
	if s == "" {
		return 0
	}
	var f float64
	if _, err := fmt.Sscanf(s, "%g", &f); err != nil {
		return 0
	}
	return f
}

// purchaseModel normalizes the AWS lifecycle field ("spot", or empty for
// on-demand) into the label the metrics and the UI both use.
func purchaseModel(lifecycle string) string {
	if strings.EqualFold(lifecycle, "spot") {
		return "spot"
	}
	return "ondemand"
}

// metricsAllowed decides whether a scrape may read the meter. The endpoint
// carries instance names, owners and spend, and it sits outside the session
// middleware, so it needs its own gate:
//
//   - with EC2CP_METRICS_TOKEN set, the bearer token is the whole check;
//   - without one, only a direct loopback connection is served. nginx proxies
//     from loopback too, but a proxied request carries X-Forwarded-For, which
//     is what separates "Prometheus on this host" from "the public /ec2 vhost".
func metricsAllowed(r *http.Request, token string) bool {
	if token != "" {
		return r.Header.Get("Authorization") == "Bearer "+token
	}
	if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Real-IP") != "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// escapeLabel quotes a Prometheus label value: backslash, quote and newline
// are the only characters the exposition format cares about.
func escapeLabel(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

// snapshot returns the accounts sorted by id, so the exposition output is
// stable and diffable.
func (m *costMeter) snapshot() ([]string, map[string]meterAccount) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.accounts))
	out := make(map[string]meterAccount, len(m.accounts))
	for id, a := range m.accounts {
		ids = append(ids, id)
		out[id] = *a
	}
	sort.Strings(ids)
	return ids, out
}

// handleMetrics serves the Prometheus text exposition format. Hand-written
// rather than pulled from client_golang: five series with no histograms is not
// worth the dependency, and the format is stable.
func (m *costMeter) handleMetrics() http.HandlerFunc {
	token := os.Getenv("EC2CP_METRICS_TOKEN")
	return func(w http.ResponseWriter, r *http.Request) {
		if !metricsAllowed(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ids, accounts := m.snapshot()
		var b strings.Builder

		labels := func(id string, a meterAccount, full bool) string {
			owner := a.Owner
			if owner == "" {
				owner = "unassigned"
			}
			// escapeLabel already produced the quoted form; %q would escape it
			// a second time.
			label := func(name, value string) string {
				return name + `="` + escapeLabel(value) + `"`
			}
			// "sandbox", not "instance": Prometheus reserves `instance` for the
			// scrape target and renames a colliding exposed label to
			// `exported_instance`, which silently collapses every per-instance
			// query into one series.
			parts := []string{label("sandbox", id), label("owner", owner)}
			if full {
				parts = append(parts, label("type", a.Type), label("model", a.Model), label("az", a.AZ))
			}
			return "{" + strings.Join(parts, ",") + "}"
		}

		emit := func(name, help, typ string, full bool, value func(meterAccount) float64) {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
			for _, id := range ids {
				a := accounts[id]
				fmt.Fprintf(&b, "%s%s %g\n", name, labels(id, a, full), value(a))
			}
		}

		emit("ec2cp_instance_running", "1 while the instance is running.", "gauge", true,
			func(a meterAccount) float64 {
				if a.Running {
					return 1
				}
				return 0
			})
		emit("ec2cp_instance_hourly_usd", "Approximate hourly price of the running instance, 0 when down.", "gauge", true,
			func(a meterAccount) float64 { return a.Rate })
		emit("ec2cp_instance_cost_usd_total", "Approximate cost accrued while metered.", "counter", false,
			func(a meterAccount) float64 { return a.CostUSD })
		emit("ec2cp_instance_running_seconds_total", "Seconds observed in the running state.", "counter", false,
			func(a meterAccount) float64 { return a.Seconds })
		emit("ec2cp_instance_sessions_total", "Transitions from stopped to running.", "counter", false,
			func(a meterAccount) float64 { return float64(a.Sessions) })

		m.mu.RLock()
		last := m.lastTick
		m.mu.RUnlock()
		fmt.Fprintf(&b, "# HELP ec2cp_meter_last_tick_timestamp_seconds Unix time of the last metering tick.\n")
		fmt.Fprintf(&b, "# TYPE ec2cp_meter_last_tick_timestamp_seconds gauge\n")
		fmt.Fprintf(&b, "ec2cp_meter_last_tick_timestamp_seconds %d\n", last.Unix())

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(b.String()))
	}
}
