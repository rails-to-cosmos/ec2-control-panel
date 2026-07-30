package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ec2cp/src/config"
)

// userRegistry points the registry at a temp file holding BODY.
func userRegistry(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "users.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EC2CP_USER_DB", path)
	// The admin cache keys off the file's identity, so a fresh path is enough to
	// invalidate it between tests.
	return path
}

func patchAdmin(t *testing.T, auth *AuthConfig, target, by, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PATCH", "/api/users/"+target, strings.NewReader(body))
	req.SetPathValue("username", target)
	if by != "" {
		req = req.WithContext(context.WithValue(req.Context(), userCtxKey{}, by))
	}
	rec := httptest.NewRecorder()
	handleUserAdmin(auth)(rec, req)
	return rec
}

func adminFlag(t *testing.T, username string) bool {
	t.Helper()
	users, err := config.LoadUsers()
	if err != nil {
		t.Fatal(err)
	}
	return users[username].Admin
}

func TestGrantAndRevokeAdmin(t *testing.T) {
	userRegistry(t, `{"bob": {"source": "oauth"}, "boss": {"source": "oauth"}}`)
	auth := &AuthConfig{admins: map[string]bool{"boss": true}}

	if rec := patchAdmin(t, auth, "bob", "boss", `{"admin": true}`); rec.Code != 200 {
		t.Fatalf("grant = %d: %s", rec.Code, rec.Body)
	}
	if !adminFlag(t, "bob") {
		t.Error("grant did not persist")
	}
	// The new admin is recognised without a restart.
	if !auth.isAdmin("bob") {
		t.Error("isAdmin does not see the registry grant")
	}
	// And the grant is attributed, for the audit trail.
	users, _ := config.LoadUsers()
	if users["bob"].AdminBy != "boss" || users["bob"].AdminAt == "" {
		t.Errorf("grant not attributed: %+v", users["bob"])
	}

	if rec := patchAdmin(t, auth, "bob", "boss", `{"admin": false}`); rec.Code != 200 {
		t.Fatalf("revoke = %d: %s", rec.Code, rec.Body)
	}
	if adminFlag(t, "bob") {
		t.Error("revoke did not persist")
	}
	if auth.isAdmin("bob") {
		t.Error("isAdmin still sees a revoked grant")
	}
}

// Nobody may strip their own rights: the same reasoning as the readers
// self-lockout guard.
func TestAdminCannotRevokeSelf(t *testing.T) {
	userRegistry(t, `{"bob": {"source": "oauth", "admin": true}}`)
	auth := &AuthConfig{}

	rec := patchAdmin(t, auth, "bob", "bob", `{"admin": false}`)
	if rec.Code != 409 {
		t.Errorf("self-revoke = %d, want 409", rec.Code)
	}
	if !adminFlag(t, "bob") {
		t.Error("self-revoke went through anyway")
	}
}

// EC2CP_ADMINS is the bootstrap set. Revoking it here would report success and
// change nothing, since isAdmin unions the env list.
func TestEnvAdminCannotBeRevoked(t *testing.T) {
	userRegistry(t, `{"boss": {"source": "oauth"}}`)
	auth := &AuthConfig{admins: map[string]bool{"boss": true}}

	rec := patchAdmin(t, auth, "boss", "someone", `{"admin": false}`)
	if rec.Code != 409 {
		t.Errorf("env-admin revoke = %d, want 409", rec.Code)
	}
	if !auth.isAdmin("boss") {
		t.Error("env admin lost their rights")
	}
}

func TestAdminPatchValidation(t *testing.T) {
	userRegistry(t, `{"bob": {"source": "oauth"}}`)
	auth := &AuthConfig{}

	// A missing "admin" must not be read as false and silently revoke.
	if rec := patchAdmin(t, auth, "bob", "boss", `{}`); rec.Code != 400 {
		t.Errorf("no admin field = %d, want 400", rec.Code)
	}
	// An unknown user would otherwise become a phantom admin record.
	if rec := patchAdmin(t, auth, "nobody", "boss", `{"admin": true}`); rec.Code != 400 {
		t.Errorf("unknown user = %d, want 400", rec.Code)
	}
	// Identities are normalized the same way the ACLs normalize them.
	rec := patchAdmin(t, auth, "BOB@example.com", "boss", `{"admin": true}`)
	if rec.Code != 200 {
		t.Fatalf("normalized target = %d: %s", rec.Code, rec.Body)
	}
	var got struct{ Username string }
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Username != "bob" {
		t.Errorf("target normalized to %q, want bob", got.Username)
	}
}

// The list must not leak who is an admin to non-admins, and must mark env-derived
// rights so the UI can lock that checkbox.
func TestUsersListAdminFields(t *testing.T) {
	userRegistry(t, `{"bob": {"source": "oauth", "admin": true}, "boss": {"source": "oauth"}}`)
	auth := &AuthConfig{admins: map[string]bool{"boss": true}}

	fetch := func(as string) []map[string]any {
		req := httptest.NewRequest("GET", "/api/users", nil)
		req = req.WithContext(context.WithValue(req.Context(), userCtxKey{}, as))
		rec := httptest.NewRecorder()
		handleUsers(auth)(rec, req)
		var body struct{ Users []map[string]any }
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Users
	}

	for _, u := range fetch("boss") {
		switch u["username"] {
		case "bob":
			if u["admin"] != true || u["envAdmin"] == true {
				t.Errorf("bob: %+v — want a registry grant", u)
			}
		case "boss":
			if u["admin"] != true || u["envAdmin"] != true {
				t.Errorf("boss: %+v — want an env grant", u)
			}
		}
	}
	for _, u := range fetch("bystander") {
		if _, ok := u["admin"]; ok {
			t.Errorf("non-admin sees admin flags: %+v", u)
		}
	}
}
