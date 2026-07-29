package server

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ec2cp/src/ec2"
)

// fakeSnapshots stands in for the status cache.
type fakeSnapshots map[string]*ec2.Snapshot

func (f fakeSnapshots) Get(id string) *ec2.Snapshot { return f[id] }

func running(instType, lifecycle, az string) *ec2.Snapshot {
	return &ec2.Snapshot{
		AZ:       az,
		Instance: &ec2.InstanceDetails{State: "running", InstanceType: instType, Lifecycle: lifecycle},
	}
}

// writeInstances puts an instances.json in a temp cwd, which is where
// config.LoadInstances looks.
func writeInstances(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "instances.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
}

func newTestMeter(t *testing.T, snaps fakeSnapshots, rate float64) *costMeter {
	t.Helper()
	m := newCostMeter(nil, snaps, time.Minute, "")
	m.rate = func(context.Context, string, string, string) float64 { return rate }
	return m
}

// tickAfter runs a tick as if SINCE had passed since the previous one.
func tickAfter(m *costMeter, since time.Duration) {
	if since > 0 {
		m.mu.Lock()
		m.lastTick = time.Now().Add(-since)
		m.mu.Unlock()
	}
	m.tick(context.Background())
}

func TestMeterAccrual(t *testing.T) {
	writeInstances(t, `{"box": {"owner": "bob"}}`)
	m := newTestMeter(t, fakeSnapshots{"box": running("r7i.2xlarge", "spot", "az-1")}, 3.60)

	// The first tick has no previous one to measure against.
	m.tick(context.Background())
	a := m.accounts["box"]
	if a.CostUSD != 0 || a.Seconds != 0 {
		t.Fatalf("first tick billed %v USD / %v s, want 0", a.CostUSD, a.Seconds)
	}
	if a.Sessions != 1 {
		t.Errorf("sessions = %d, want 1 (stopped -> running)", a.Sessions)
	}

	tickAfter(m, time.Minute)
	if want := 0.06; a.CostUSD < want-1e-6 || a.CostUSD > want+1e-6 {
		t.Errorf("cost after a minute at $3.60/h = %v, want %v", a.CostUSD, want)
	}
	if a.Seconds < 59 || a.Seconds > 61 {
		t.Errorf("seconds = %v, want ~60", a.Seconds)
	}
	if a.Owner != "bob" || a.Type != "r7i.2xlarge" || a.Model != "spot" || a.AZ != "az-1" {
		t.Errorf("labels = %+v", a)
	}
	if a.Sessions != 1 {
		t.Errorf("sessions = %d, want 1 — still the same run", a.Sessions)
	}
}

func TestMeterIgnoresStopped(t *testing.T) {
	writeInstances(t, `{"box": {"owner": "bob"}}`)
	snaps := fakeSnapshots{"box": nil}
	m := newTestMeter(t, snaps, 3.60)

	m.tick(context.Background())
	tickAfter(m, time.Minute)
	a := m.accounts["box"]
	if a.CostUSD != 0 || a.Seconds != 0 || a.Rate != 0 {
		t.Fatalf("stopped instance billed %+v", a)
	}
	if a.Sessions != 0 {
		t.Errorf("sessions = %d, want 0", a.Sessions)
	}

	// Starting it counts one session; stopping and starting again counts two.
	snaps["box"] = running("m5.large", "", "az-1")
	tickAfter(m, time.Minute)
	snaps["box"] = nil
	tickAfter(m, time.Minute)
	snaps["box"] = running("m5.large", "", "az-1")
	tickAfter(m, time.Minute)
	if a.Sessions != 2 {
		t.Errorf("sessions = %d, want 2", a.Sessions)
	}
	if a.Model != "ondemand" {
		t.Errorf("model = %q, want ondemand for an empty lifecycle", a.Model)
	}
}

// A gap longer than a few intervals means the process was down. Billing it
// would invent hours the meter never observed.
func TestMeterSkipsDowntimeGap(t *testing.T) {
	writeInstances(t, `{"box": {"owner": "bob"}}`)
	m := newTestMeter(t, fakeSnapshots{"box": running("m5.large", "spot", "az-1")}, 3.60)

	m.tick(context.Background())
	tickAfter(m, time.Hour)
	if a := m.accounts["box"]; a.CostUSD != 0 || a.Seconds != 0 {
		t.Errorf("an hour-long gap billed %v USD / %v s, want 0", a.CostUSD, a.Seconds)
	}
}

func TestMeterDropsRemovedInstances(t *testing.T) {
	writeInstances(t, `{"box": {"owner": "bob"}}`)
	m := newTestMeter(t, fakeSnapshots{"box": running("m5.large", "spot", "az-1")}, 1)
	m.tick(context.Background())
	if _, ok := m.accounts["box"]; !ok {
		t.Fatal("no account for a known instance")
	}

	writeInstances(t, `{"other": {}}`)
	m.tick(context.Background())
	if _, ok := m.accounts["box"]; ok {
		t.Error("account survived removal from instances.json")
	}
}

func TestMeterExposition(t *testing.T) {
	writeInstances(t, `{"box": {"owner": "bob"}, "quoted\"name": {}}`)
	m := newTestMeter(t, fakeSnapshots{"box": running("r7i.2xlarge", "spot", "az-1")}, 3.60)
	m.tick(context.Background())
	tickAfter(m, time.Minute)

	rec := httptest.NewRecorder()
	scrape := httptest.NewRequest("GET", metricsPath, nil)
	scrape.RemoteAddr = "127.0.0.1:54321" // the endpoint is loopback-only
	m.handleMetrics()(rec, scrape)
	body := rec.Body.String()

	for _, want := range []string{
		`# TYPE ec2cp_instance_running gauge`,
		`ec2cp_instance_running{instance="box",owner="bob",type="r7i.2xlarge",model="spot",az="az-1"} 1`,
		`ec2cp_instance_hourly_usd{instance="box",owner="bob",type="r7i.2xlarge",model="spot",az="az-1"} 3.6`,
		`# TYPE ec2cp_instance_cost_usd_total counter`,
		// Wall-clock elapsed carries a few microseconds of slop, so the value is
		// 0.06000000…; the prefix is what matters.
		`ec2cp_instance_cost_usd_total{instance="box",owner="bob"} 0.06`,
		`ec2cp_instance_sessions_total{instance="box",owner="bob"} 1`,
		// An instance with no owner still needs a label value.
		`owner="unassigned"`,
		// Label values are quoted, so a quote in a name has to be escaped.
		`instance="quoted\"name"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q\n---\n%s", want, body)
		}
	}
}

// Without a token the endpoint is loopback-only, and a proxied request is not
// loopback however it arrives: /metrics sits outside the session middleware, so
// the public /ec2 vhost must not be able to read spend per user.
func TestMetricsLoopbackOnlyWithoutToken(t *testing.T) {
	writeInstances(t, `{}`)
	m := newTestMeter(t, fakeSnapshots{}, 0)
	h := m.handleMetrics()

	direct := httptest.NewRequest("GET", metricsPath, nil) // RemoteAddr 192.0.2.1
	direct.RemoteAddr = "127.0.0.1:54321"
	rec := httptest.NewRecorder()
	h(rec, direct)
	if rec.Code != 200 {
		t.Errorf("loopback scrape = %d, want 200", rec.Code)
	}

	proxied := httptest.NewRequest("GET", metricsPath, nil)
	proxied.RemoteAddr = "127.0.0.1:54321"
	proxied.Header.Set("X-Forwarded-For", "203.0.113.7")
	rec = httptest.NewRecorder()
	h(rec, proxied)
	if rec.Code != 401 {
		t.Errorf("proxied scrape = %d, want 401", rec.Code)
	}

	remote := httptest.NewRequest("GET", metricsPath, nil)
	remote.RemoteAddr = "10.17.5.20:33333"
	rec = httptest.NewRecorder()
	h(rec, remote)
	if rec.Code != 401 {
		t.Errorf("off-host scrape = %d, want 401", rec.Code)
	}
}

func TestMetricsTokenGate(t *testing.T) {
	writeInstances(t, `{}`)
	t.Setenv("EC2CP_METRICS_TOKEN", "s3cret")
	m := newTestMeter(t, fakeSnapshots{}, 0)
	h := m.handleMetrics()

	// With a token set, even a loopback scrape has to present it.
	rec := httptest.NewRecorder()
	noToken := httptest.NewRequest("GET", metricsPath, nil)
	noToken.RemoteAddr = "127.0.0.1:54321"
	h(rec, noToken)
	if rec.Code != 401 {
		t.Errorf("unauthenticated scrape = %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", metricsPath, nil)
	req.RemoteAddr = "10.17.5.20:33333"
	req.Header.Set("Authorization", "Bearer s3cret")
	h(rec, req)
	if rec.Code != 200 {
		t.Errorf("authenticated scrape = %d, want 200", rec.Code)
	}
}

func TestMeterStatePersistsCounters(t *testing.T) {
	writeInstances(t, `{"box": {"owner": "bob"}}`)
	dir := t.TempDir()
	path := filepath.Join(dir, "cost-meter.json")

	m := newTestMeter(t, fakeSnapshots{"box": running("m5.large", "spot", "az-1")}, 3.60)
	m.statePath = path
	m.tick(context.Background())
	tickAfter(m, time.Minute)
	m.save()

	// A deploy restarts the process; the counters must not go backwards or
	// Prometheus reads it as a counter reset and loses the accrued cost.
	next := newTestMeter(t, fakeSnapshots{}, 0)
	next.statePath = path
	next.load()
	a, ok := next.accounts["box"]
	if !ok {
		t.Fatal("counters did not survive a restart")
	}
	if a.CostUSD < 0.059 || a.CostUSD > 0.061 {
		t.Errorf("restored cost = %v, want ~0.06", a.CostUSD)
	}
	if a.Running || a.Rate != 0 {
		t.Errorf("restored account claims to be running: %+v", a)
	}
}

func TestMeterStatePathBesideStatusCache(t *testing.T) {
	if got, want := meterStatePath("state/status-cache.json"), filepath.Join("state", "cost-meter.json"); got != want {
		t.Errorf("meterStatePath = %q, want %q", got, want)
	}
	if got := meterStatePath(""); got != "" {
		t.Errorf("meterStatePath(\"\") = %q, want empty (persistence off)", got)
	}
}
