package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// grafanaEcho stands in for Grafana, reporting the path and identity headers it
// was handed.
func grafanaEcho(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Seen-Path", r.URL.Path)
		w.Header().Set("X-Seen-User", r.Header.Get(grafanaUserHeader))
		w.Header().Set("X-Seen-Role", r.Header.Get(grafanaRoleHeader))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func grafanaRequest(t *testing.T, auth *AuthConfig, upstream, path, user string, hdr http.Header) *httptest.ResponseRecorder {
	t.Helper()
	h, err := handleGrafana(auth, upstream)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", path, nil)
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if user != "" {
		req = req.WithContext(context.WithValue(req.Context(), userCtxKey{}, user))
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// Grafana runs with serve_from_sub_path, so it has to receive the public path.
// nginx strips the prefix before this app sees it; the proxy puts it back.
func TestGrafanaProxyRestoresBasePath(t *testing.T) {
	up := grafanaEcho(t)
	auth := &AuthConfig{basePath: "/ec2"}

	rec := grafanaRequest(t, auth, up.URL, "/grafana/d/ec2cp-spend-per-user/ec2-costs", "bob", nil)
	if got, want := rec.Header().Get("X-Seen-Path"), "/ec2/grafana/d/ec2cp-spend-per-user/ec2-costs"; got != want {
		t.Errorf("upstream saw path %q, want %q", got, want)
	}
	if got := rec.Header().Get("X-Seen-User"); got != "bob" {
		t.Errorf("upstream saw user %q, want bob", got)
	}
	if got := rec.Header().Get("X-Seen-Role"); got != "Admin" {
		t.Errorf("upstream saw role %q, want Admin", got)
	}
}

// The auth-proxy headers ARE the authentication as far as Grafana is concerned,
// so a client-supplied one must be discarded rather than forwarded or merged.
func TestGrafanaProxyDropsClientIdentityHeaders(t *testing.T) {
	up := grafanaEcho(t)
	auth := &AuthConfig{basePath: "/ec2"}

	spoof := http.Header{}
	spoof.Set(grafanaUserHeader, "attacker")
	spoof.Set(grafanaRoleHeader, "Admin")

	rec := grafanaRequest(t, auth, up.URL, "/grafana/", "bob", spoof)
	if got := rec.Header().Get("X-Seen-User"); got != "bob" {
		t.Errorf("upstream saw user %q, want bob — a client header won", got)
	}

	// No session at all: nothing may be asserted upstream. (The route guard
	// rejects this case first; this is the belt to that braces.)
	rec = grafanaRequest(t, auth, up.URL, "/grafana/", "", spoof)
	if got := rec.Header().Get("X-Seen-User"); got != "" {
		t.Errorf("upstream saw user %q with no session, want none", got)
	}
	if got := rec.Header().Get("X-Seen-Role"); got != "" {
		t.Errorf("upstream saw role %q with no session, want none", got)
	}
}

// A non-admin must not reach the dashboards: they aggregate every user's spend.
func TestGrafanaRouteIsAdminOnly(t *testing.T) {
	up := grafanaEcho(t)
	auth := &AuthConfig{basePath: "/ec2", admins: map[string]bool{"boss": true}}
	h, err := handleGrafana(auth, up.URL)
	if err != nil {
		t.Fatal(err)
	}
	guarded := auth.wrap(guardAdmin, h)

	for _, tc := range []struct{ user string; want int }{
		{"boss", 200},
		{"bob", 403},
		{"", 403},
	} {
		req := httptest.NewRequest("GET", "/grafana/", nil)
		if tc.user != "" {
			req = req.WithContext(context.WithValue(req.Context(), userCtxKey{}, tc.user))
		}
		rec := httptest.NewRecorder()
		guarded(rec, req)
		if rec.Code != tc.want {
			t.Errorf("user %q got %d, want %d", tc.user, rec.Code, tc.want)
		}
	}
}
