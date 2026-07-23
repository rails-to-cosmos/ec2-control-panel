// Auth gate for the ec2cp web UI: Google OAuth2 (authorization-code) and/or
// username+password, sharing one stateless HMAC-signed session cookie.
//
// The Google identity's email local-part is used as the username (so it matches
// the dotted usernames in instances.json `readers` / EC2CP_ADMINS).
//
// Env vars (all optional — auth is off unless at least one method is set):
//
//	GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET, OAUTH_CALLBACK_URL
//	              enable Google OAuth. OAUTH_ALLOWED_USERS (csv of usernames)
//	              tightens the allowlist; empty means any user who passes
//	              OAUTH_ALLOWED_DOMAIN. OAUTH_ALLOWED_DOMAIN restricts to a
//	              Google Workspace hosted domain (strongly recommended).
//	OAUTH_ENABLED=false disables OAuth even when the above are set.
//	EC2CP_USERS   "user:pbkdf2_sha256$iters$salt$hash,..." enables password login.
//	EC2CP_COOKIE_SECRET  session-signing key; ephemeral (resets on restart) if unset.
//	EC2CP_BASE_PATH      external mount prefix (e.g. "/ec2") so redirects, links
//	              and the session cookie use the right path behind a subpath proxy.
package server

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"ec2cp/src/config"
)

// userCtxKey carries the authenticated username through the request context.
type userCtxKey struct{}

// UserFromContext returns the authenticated username, or "" if none.
func UserFromContext(ctx context.Context) string {
	u, _ := ctx.Value(userCtxKey{}).(string)
	return u
}

const (
	sessionCookieName = "ec2cp_session"
	defaultSessionTTL = 8 * time.Hour
	stateTTL          = 10 * time.Minute
	oauthScope        = "openid email profile"
	oauthHTTPTimeout  = 10 * time.Second

	googleAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL    = "https://oauth2.googleapis.com/token"
	googleUserInfoURL = "https://openidconnect.googleapis.com/v1/userinfo"

	pbkdf2Algorithm  = "pbkdf2_sha256"
	pbkdf2Iterations = 240_000
	pbkdf2SaltBytes  = 16
	pbkdf2KeyBytes   = 32
)

var b64 = base64.RawURLEncoding

// dummyPasswordHash is a valid-but-unmatchable encoding used to equalise
// verify timing for unknown usernames (avoids leaking which accounts exist).
var dummyPasswordHash = HashPassword(randToken(16))

// envFlag parses a boolean env var: unset → DEF; else anything but a falsey token is true.
func envFlag(name string, def bool) bool {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return b64.EncodeToString(b)
}

// sign returns "<b64(payload)>.<b64(hmac-sha256)>" for a JSON payload.
func sign(secret []byte, payload map[string]any) string {
	raw, _ := json.Marshal(payload) // map[string]any never fails to marshal
	body := b64.EncodeToString(raw)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(body))
	return body + "." + b64.EncodeToString(mac.Sum(nil))
}

// unsign verifies the signature and exp; returns the payload and true if valid.
func unsign(secret []byte, token string) (map[string]any, bool) {
	dot := strings.IndexByte(token, '.')
	if dot < 0 {
		return nil, false
	}
	body, sig := token[:dot], token[dot+1:]
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(body))
	expected := b64.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return nil, false
	}
	raw, err := b64.DecodeString(body)
	if err != nil {
		return nil, false
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, false
	}
	if exp, ok := data["exp"].(float64); ok && exp < float64(time.Now().Unix()) {
		return nil, false
	}
	return data, true
}

// HashPassword encodes PASSWORD as pbkdf2_sha256$<iters>$<salt>$<hash>.
func HashPassword(password string) string {
	salt := make([]byte, pbkdf2SaltBytes)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	derived, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, pbkdf2KeyBytes)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%s$%d$%s$%s", pbkdf2Algorithm, pbkdf2Iterations, b64.EncodeToString(salt), b64.EncodeToString(derived))
}

// verifyPassword does a constant-time check of PASSWORD against an encoded
// PBKDF2 hash. Returns false (never panics) for any malformed encoding.
func verifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != pbkdf2Algorithm {
		return false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations <= 0 {
		return false
	}
	salt, err := b64.DecodeString(parts[2])
	if err != nil {
		return false
	}
	expected, err := b64.DecodeString(parts[3])
	if err != nil || len(expected) == 0 {
		return false
	}
	derived, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(expected))
	if err != nil {
		return false
	}
	return hmac.Equal(derived, expected)
}

// parseUsers parses EC2CP_USERS ("user:hash,user2:hash2") into {user: hash}.
// PBKDF2 encodings never contain "," and usernames never contain ":", so
// splitting on the first ":" per comma-entry is safe. Malformed entries drop.
func parseUsers(raw string) map[string]string {
	users := map[string]string{}
	for _, entry := range strings.Split(raw, ",") {
		user, encoded, found := strings.Cut(strings.TrimSpace(entry), ":")
		user, encoded = strings.TrimSpace(user), strings.TrimSpace(encoded)
		if found && user != "" && encoded != "" {
			users[user] = encoded
		}
	}
	return users
}

func resolveCookieSecret() []byte {
	if s := os.Getenv("EC2CP_COOKIE_SECRET"); s != "" {
		return []byte(s)
	}
	fmt.Println("ec2cp: EC2CP_COOKIE_SECRET unset; generated an ephemeral secret (sessions reset on restart)")
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// OAuthConfig holds Google OAuth2 settings resolved from the environment.
type OAuthConfig struct {
	ClientID      string
	ClientSecret  string
	CallbackURL   string
	AllowedUsers  map[string]bool // empty → any user passing AllowedDomain
	AllowedDomain string          // Google Workspace hosted domain; empty → no domain check
}

func loadOAuthConfig() *OAuthConfig {
	if !envFlag("OAUTH_ENABLED", true) {
		return nil
	}
	id := os.Getenv("GOOGLE_CLIENT_ID")
	secret := os.Getenv("GOOGLE_CLIENT_SECRET")
	cb := os.Getenv("OAUTH_CALLBACK_URL")
	if id == "" || secret == "" || cb == "" {
		return nil
	}
	allowed := map[string]bool{}
	for _, u := range strings.Split(os.Getenv("OAUTH_ALLOWED_USERS"), ",") {
		if u = strings.TrimSpace(u); u != "" {
			allowed[u] = true
		}
	}
	return &OAuthConfig{
		ClientID:      id,
		ClientSecret:  secret,
		CallbackURL:   cb,
		AllowedUsers:  allowed,
		AllowedDomain: strings.TrimSpace(os.Getenv("OAUTH_ALLOWED_DOMAIN")),
	}
}

// AuthConfig is the web UI's auth surface: a shared signed-cookie session plus
// the sign-in methods (Google OAuth and/or password) that mint it. nil means
// no method is configured and the UI runs unauthenticated.
type AuthConfig struct {
	cookieSecret []byte
	oauth        *OAuthConfig
	users        map[string]string
	admins       map[string]bool // usernames with access to every instance
	ttl          time.Duration
	basePath     string // external mount prefix, e.g. "/ec2" (no trailing slash)
}

// LoadAuthConfig builds auth from the environment, or nil if no method is set.
func LoadAuthConfig() *AuthConfig {
	oauth := loadOAuthConfig()
	users := parseUsers(os.Getenv("EC2CP_USERS"))
	if oauth == nil && len(users) == 0 {
		return nil
	}
	admins := map[string]bool{}
	for _, u := range strings.Split(os.Getenv("EC2CP_ADMINS"), ",") {
		if u = strings.TrimSpace(u); u != "" {
			admins[u] = true
		}
	}
	return &AuthConfig{
		cookieSecret: resolveCookieSecret(),
		oauth:        oauth,
		users:        users,
		admins:       admins,
		ttl:          defaultSessionTTL,
		basePath:     strings.TrimRight(os.Getenv("EC2CP_BASE_PATH"), "/"),
	}
}

func (a *AuthConfig) oauthEnabled() bool       { return a.oauth != nil }
func (a *AuthConfig) passwordEnabled() bool    { return len(a.users) > 0 }
func (a *AuthConfig) isAdmin(user string) bool { return user != "" && a.admins[user] }

// p prefixes an app-internal path with the external base path.
func (a *AuthConfig) p(path string) string { return a.basePath + path }

func (a *AuthConfig) cookiePath() string {
	if a.basePath == "" {
		return "/"
	}
	return a.basePath
}

// currentUser returns the username from a valid session cookie, else "".
func (a *AuthConfig) currentUser(r *http.Request) string {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	data, ok := unsign(a.cookieSecret, c.Value)
	if !ok {
		return ""
	}
	if u, ok := data["user"].(string); ok {
		return u
	}
	return ""
}

// issueSession sets the signed session cookie and 302s to next.
func (a *AuthConfig) issueSession(w http.ResponseWriter, r *http.Request, username, next string) {
	token := sign(a.cookieSecret, map[string]any{
		"user": username,
		"exp":  time.Now().Add(a.ttl).Unix(),
	})
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     a.cookiePath(),
		MaxAge:   int(a.ttl.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, next, http.StatusFound)
}

// safeNext constrains a post-login redirect to a local path (open-redirect guard).
func (a *AuthConfig) safeNext(raw string) string {
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") {
		return raw
	}
	return a.p("/")
}

// loginURL is the external /login URL carrying a post-login destination + error.
func (a *AuthConfig) loginURL(next, errMsg string) string {
	q := url.Values{"next": {next}}
	if errMsg != "" {
		q.Set("error", errMsg)
	}
	return a.p("/login") + "?" + q.Encode()
}

// middleware requires a valid session on every path except the public ones,
// and stashes the authenticated username in the request context.
func (a *AuthConfig) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/login" || strings.HasPrefix(path, "/oauth/") || strings.HasPrefix(path, "/assets/") {
			next.ServeHTTP(w, r)
			return
		}
		user := a.currentUser(r)
		if user == "" {
			ext := a.basePath + r.URL.Path
			if r.URL.RawQuery != "" {
				ext += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, a.loginURL(ext, ""), http.StatusFound)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userCtxKey{}, user)))
	})
}

// RequireInstanceAccess wraps a per-instance handler, returning 403 when the
// authenticated user is not a reader of the instance named by the {id} path
// value. No-op when auth is disabled (a == nil).
func (a *AuthConfig) RequireInstanceAccess(next http.HandlerFunc) http.HandlerFunc {
	if a == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		insts, err := config.LoadInstances()
		if err == nil {
			if inst, ok := insts[id]; ok {
				user := UserFromContext(r.Context())
				if !inst.CanRead(user, a.isAdmin(user)) {
					http.Error(w, "forbidden: not authorized for this instance", http.StatusForbidden)
					return
				}
			}
		}
		next(w, r)
	}
}

// registerAuthRoutes attaches the auth endpoints to the mux (app-internal paths;
// a subpath proxy strips the base prefix before they arrive).
func (a *AuthConfig) registerAuthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", a.handleLoginGet)
	mux.HandleFunc("POST /login", a.handleLoginPost)
	mux.HandleFunc("GET /oauth/login", a.handleOAuthLogin)
	mux.HandleFunc("GET /oauth/callback", a.handleOAuthCallback)
	mux.HandleFunc("GET /logout", a.handleLogout)
}

func (a *AuthConfig) handleLoginGet(w http.ResponseWriter, r *http.Request) {
	next := a.safeNext(r.URL.Query().Get("next"))
	if a.currentUser(r) != "" {
		http.Redirect(w, r, next, http.StatusFound)
		return
	}
	a.renderLoginPage(w, next, r.URL.Query().Get("error"))
}

func (a *AuthConfig) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	next := a.safeNext(r.PostFormValue("next"))

	encoded, ok := a.users[username]
	if !ok {
		verifyPassword(password, dummyPasswordHash) // equalise timing
		http.Redirect(w, r, a.loginURL(next, "Invalid username or password."), http.StatusFound)
		return
	}
	if !verifyPassword(password, encoded) {
		http.Redirect(w, r, a.loginURL(next, "Invalid username or password."), http.StatusFound)
		return
	}
	a.issueSession(w, r, username, next)
}

func (a *AuthConfig) handleOAuthLogin(w http.ResponseWriter, r *http.Request) {
	if a.oauth == nil {
		http.NotFound(w, r)
		return
	}
	state := sign(a.cookieSecret, map[string]any{
		"next":  a.safeNext(r.URL.Query().Get("next")),
		"nonce": randToken(8),
		"exp":   time.Now().Add(stateTTL).Unix(),
	})
	q := url.Values{
		"client_id":     {a.oauth.ClientID},
		"redirect_uri":  {a.oauth.CallbackURL},
		"response_type": {"code"},
		"scope":         {oauthScope},
		"state":         {state},
		"access_type":   {"online"},
		"prompt":        {"select_account"},
	}
	if a.oauth.AllowedDomain != "" {
		q.Set("hd", a.oauth.AllowedDomain) // hint Google to the Workspace domain
	}
	http.Redirect(w, r, googleAuthURL+"?"+q.Encode(), http.StatusFound)
}

func (a *AuthConfig) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if a.oauth == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		http.Redirect(w, r, a.loginURL(a.p("/"), "Google returned: "+e), http.StatusFound)
		return
	}
	code, state := q.Get("code"), q.Get("state")
	if code == "" || state == "" {
		http.Error(w, "missing code/state", http.StatusBadRequest)
		return
	}
	stateData, ok := unsign(a.cookieSecret, state)
	if !ok {
		http.Error(w, "invalid or expired state", http.StatusBadRequest)
		return
	}
	next := a.p("/")
	if n, ok := stateData["next"].(string); ok {
		next = a.safeNext(n)
	}

	username, err := a.exchangeAndFetchUser(code)
	if err != nil {
		fmt.Println("ec2cp: oauth callback error:", err)
		http.Error(w, "google auth failed", http.StatusBadGateway)
		return
	}
	if len(a.oauth.AllowedUsers) > 0 && !a.oauth.AllowedUsers[username] {
		http.Redirect(w, r, a.loginURL(a.p("/"), fmt.Sprintf("Account %q is not authorized.", username)), http.StatusFound)
		return
	}
	a.issueSession(w, r, username, next)
}

// exchangeAndFetchUser swaps CODE for a token, reads the Google userinfo,
// enforces email verification and the optional hosted-domain, and returns the
// username (the email local-part, lowercased).
func (a *AuthConfig) exchangeAndFetchUser(code string) (string, error) {
	client := &http.Client{Timeout: oauthHTTPTimeout}

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {a.oauth.CallbackURL},
		"client_id":     {a.oauth.ClientID},
		"client_secret": {a.oauth.ClientSecret},
	}
	tokResp, err := client.PostForm(googleTokenURL, form)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer tokResp.Body.Close()
	if tokResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(tokResp.Body, 512))
		return "", fmt.Errorf("token exchange status %d: %s", tokResp.StatusCode, body)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(tokResp.Body).Decode(&tok); err != nil {
		return "", fmt.Errorf("decode token: %w", err)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("no access_token in response")
	}

	req, _ := http.NewRequest(http.MethodGet, googleUserInfoURL, nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	userResp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("userinfo request: %w", err)
	}
	defer userResp.Body.Close()
	if userResp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch userinfo status %d", userResp.StatusCode)
	}
	var u struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		HD            string `json:"hd"`
	}
	if err := json.NewDecoder(userResp.Body).Decode(&u); err != nil {
		return "", fmt.Errorf("decode userinfo: %w", err)
	}
	if u.Email == "" {
		return "", fmt.Errorf("no email in userinfo")
	}
	if !u.EmailVerified {
		return "", fmt.Errorf("email %q not verified", u.Email)
	}
	at := strings.LastIndex(u.Email, "@")
	if at <= 0 {
		return "", fmt.Errorf("malformed email %q", u.Email)
	}
	if a.oauth.AllowedDomain != "" {
		domain := u.HD
		if domain == "" {
			domain = u.Email[at+1:]
		}
		if !strings.EqualFold(domain, a.oauth.AllowedDomain) {
			return "", fmt.Errorf("domain %q not allowed", domain)
		}
	}
	return strings.ToLower(u.Email[:at]), nil
}

func (a *AuthConfig) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     a.cookiePath(),
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, a.p("/"), http.StatusFound)
}

var loginTmpl = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sign in · EC2 Control Panel</title>
<style>
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
    background: #f8f9fa; color: #333; display: flex; min-height: 100vh; margin: 0; align-items: center; justify-content: center; }
  .card { background: #fff; padding: 2rem 2.25rem; border-radius: 10px; box-shadow: 0 6px 20px rgba(0,0,0,.1); width: 320px; }
  h1 { font-size: 1.25rem; margin: 0 0 .25rem; color: #2c3e50; }
  .subtle { color: #6c757d; font-size: .85rem; }
  form { margin-top: 1.25rem; display: flex; flex-direction: column; gap: .6rem; }
  input[type=text], input[type=password] { padding: .55rem .7rem; border: 1px solid #ced4da; border-radius: 6px; font-size: .95rem; }
  button { padding: .55rem .7rem; border: 0; border-radius: 6px; font-size: .95rem; cursor: pointer; }
  .primary { background: #2563eb; color: #fff; }
  .google { display: block; text-align: center; text-decoration: none; padding: .6rem; border-radius: 6px;
    background: #4285f4; color: #fff; font-weight: 600; margin-top: 1rem; }
  .sep { text-align: center; color: #adb5bd; font-size: .8rem; margin: 1rem 0 .25rem; }
  .error { background: #fdecea; color: #b3261e; border: 1px solid #f5c6cb; padding: .5rem .7rem;
    border-radius: 6px; font-size: .85rem; margin-top: 1rem; }
</style>
</head>
<body>
<div class="card">
  <h1>EC2 Control Panel</h1>
  <span class="subtle">Sign in to continue</span>
  {{if .Error}}<div class="error">{{.Error}}</div>{{end}}
  {{if .PasswordEnabled}}
  <form method="post" action="{{.Base}}/login">
    <input type="hidden" name="next" value="{{.Next}}">
    <input type="text" name="username" placeholder="Username" autocomplete="username" autofocus>
    <input type="password" name="password" placeholder="Password" autocomplete="current-password">
    <button type="submit" class="primary">Sign in</button>
  </form>
  {{end}}
  {{if and .PasswordEnabled .OAuthEnabled}}<div class="sep">— or —</div>{{end}}
  {{if .OAuthEnabled}}<a class="google" href="{{.Base}}/oauth/login?next={{.NextQuery}}">Sign in with Google</a>{{end}}
</div>
</body>
</html>`))

func (a *AuthConfig) renderLoginPage(w http.ResponseWriter, next, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = loginTmpl.Execute(w, map[string]any{
		"Base":            a.basePath,
		"Next":            next,
		"NextQuery":       next, // template URL-escapes in href context
		"Error":           errMsg,
		"OAuthEnabled":    a.oauthEnabled(),
		"PasswordEnabled": a.passwordEnabled(),
	})
}
