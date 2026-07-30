package server

import (
	"cmp"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
)

// grafanaProxyPath is where the dashboards are mounted inside this app, so a
// signed-in admin reaches Grafana on the session they already have instead of
// logging in a second time.
const grafanaProxyPath = "/grafana/"

// Headers Grafana's auth.proxy trusts. They are the whole authentication, so a
// request that arrives carrying them must have them replaced, never merged —
// otherwise any signed-in user could name themselves in a header and land in
// Grafana as someone else.
const (
	grafanaUserHeader = "X-WEBAUTH-USER"
	grafanaRoleHeader = "X-WEBAUTH-ROLE"
)

// defaultGrafanaUpstream is where this repo's compose puts Grafana, so the
// route works without another environment variable to keep in sync. It is only
// reachable over loopback, and the route is admin-gated either way.
const defaultGrafanaUpstream = "http://127.0.0.1:2726"

// grafanaUpstream resolves the Grafana address. Setting
// EC2CP_GRAFANA_UPSTREAM to "-" disables the proxy, which leaves the route
// unregistered.
func grafanaUpstream() string {
	v := cmp.Or(os.Getenv("EC2CP_GRAFANA_UPSTREAM"), defaultGrafanaUpstream)
	if v == "-" {
		return ""
	}
	return v
}

// handleGrafana reverse-proxies Grafana, asserting the caller's identity with
// the auth-proxy headers. Registered behind guardAdmin, so the *real* session
// identity has to be an admin: impersonation must not widen access, and the
// dashboards show every user's spend.
//
// Grafana runs with serve_from_sub_path, so it expects to see the public path
// (EC2CP_BASE_PATH + /grafana/...). nginx strips the base path before this app
// sees it, so the proxy puts it back.
func handleGrafana(auth *AuthConfig, upstream string) (http.HandlerFunc, error) {
	target, err := url.Parse(upstream)
	if err != nil {
		return nil, err
	}
	basePath := ""
	if auth != nil {
		basePath = auth.basePath
	}

	proxy := &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
			r.URL.Path = basePath + r.URL.Path
			r.Host = target.Host

			// Whatever the client sent for these is discarded before the
			// identity we resolved is set.
			r.Header.Del(grafanaUserHeader)
			r.Header.Del(grafanaRoleHeader)
			if auth == nil {
				return
			}
			// The real user, not reader(): an admin viewing as someone else is
			// still themselves to Grafana.
			if user := UserFromContext(r.Context()); user != "" {
				r.Header.Set(grafanaUserHeader, user)
				r.Header.Set(grafanaRoleHeader, "Admin")
			}
		},
	}
	return proxy.ServeHTTP, nil
}

// grafanaDashboardURL turns a Grafana base URL into a link straight to the cost
// dashboard, so the tab lands on the graphs instead of Grafana's home page. A
// URL that already names a dashboard is passed through untouched, which is how
// an operator points the tab somewhere else.
func grafanaDashboardURL(base string) string {
	if base == "" || strings.Contains(base, "/d/") {
		return base
	}
	return strings.TrimRight(base, "/") + "/" + costsDashboardPath
}
