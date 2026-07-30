package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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

// instanceStore points instances.json at a temp cwd holding BODY.
func instanceStore(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "instances.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
}

// A username is the only thing tying an instance to its owner and readers, so a
// rename that left those behind would silently orphan the user's own boxes.
func TestRenameCarriesInstanceReferences(t *testing.T) {
	instanceStore(t, `{
	  "theirs":  {"owner": "bob", "readers": ["bob", "alice"]},
	  "shared":  {"readers": ["alice", "bob"]},
	  "others":  {"owner": "alice", "readers": ["alice"]}
	}`)
	userRegistry(t, `{"bob": {"source": "oauth"}, "boss": {"source": "oauth"}}`)
	auth := &AuthConfig{admins: map[string]bool{"boss": true}}

	rec := patchAdmin(t, auth, "bob", "boss", `{"username": "robert"}`)
	if rec.Code != 200 {
		t.Fatalf("rename = %d: %s", rec.Code, rec.Body)
	}
	var got struct {
		Username         string
		InstancesUpdated int `json:"instancesUpdated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Username != "robert" || got.InstancesUpdated != 2 {
		t.Errorf("response = %+v, want robert / 2 instances", got)
	}

	insts, err := config.LoadInstances()
	if err != nil {
		t.Fatal(err)
	}
	if insts["theirs"].Owner != "robert" {
		t.Errorf("owner = %q, want robert", insts["theirs"].Owner)
	}
	for _, id := range []string{"theirs", "shared"} {
		if !slices.Contains(insts[id].Readers, "robert") || slices.Contains(insts[id].Readers, "bob") {
			t.Errorf("%s readers = %v, want bob replaced", id, insts[id].Readers)
		}
	}
	// Untouched entries stay untouched.
	if insts["others"].Owner != "alice" || !slices.Contains(insts["others"].Readers, "alice") {
		t.Errorf("unrelated instance changed: %+v", insts["others"])
	}
	// And the renamed user can still reach what they own.
	cfg := insts["theirs"]
	if !cfg.CanRead("robert", false) {
		t.Error("renamed owner lost access to their own instance")
	}

	users, err := config.LoadUsers()
	if err != nil {
		t.Fatal(err)
	}
	if _, present := users["bob"]; present {
		t.Error("old registry key survived the rename")
	}
	if _, ok := users["robert"]; !ok {
		t.Error("renamed record missing")
	}
}

func TestRenameRefusals(t *testing.T) {
	instanceStore(t, `{}`)
	userRegistry(t, `{"bob": {"source": "oauth"}, "alice": {"source": "oauth"}, "boss": {"source": "oauth"}}`)
	auth := &AuthConfig{admins: map[string]bool{"boss": true}}

	// Renaming yourself leaves your own session naming a user that is gone.
	if rec := patchAdmin(t, auth, "bob", "bob", `{"username": "robert"}`); rec.Code != 409 {
		t.Errorf("self-rename = %d, want 409", rec.Code)
	}
	// EC2CP_ADMINS names the old username; the rename would strip rights the API
	// cannot give back.
	if rec := patchAdmin(t, auth, "boss", "bob", `{"username": "chief"}`); rec.Code != 409 {
		t.Errorf("env-admin rename = %d, want 409", rec.Code)
	}
	// Colliding with an existing user would merge two identities.
	if rec := patchAdmin(t, auth, "bob", "boss", `{"username": "alice"}`); rec.Code != 409 {
		t.Errorf("collision = %d, want 409", rec.Code)
	}
	users, _ := config.LoadUsers()
	for _, name := range []string{"bob", "alice", "boss"} {
		if _, ok := users[name]; !ok {
			t.Errorf("%s disappeared despite the refusal", name)
		}
	}
}

func TestSetEmailAndCombinedPatch(t *testing.T) {
	instanceStore(t, `{}`)
	userRegistry(t, `{"bob": {"source": "oauth"}}`)
	auth := &AuthConfig{}

	rec := patchAdmin(t, auth, "bob", "boss", `{"email": "Bob@Example.COM", "admin": true}`)
	if rec.Code != 200 {
		t.Fatalf("combined patch = %d: %s", rec.Code, rec.Body)
	}
	users, _ := config.LoadUsers()
	if users["bob"].Email != "bob@example.com" {
		t.Errorf("email = %q, want lowercased", users["bob"].Email)
	}
	if !users["bob"].Admin {
		t.Error("admin not set by the combined patch")
	}
	// An empty patch must not be read as a silent revoke of everything.
	if rec := patchAdmin(t, auth, "bob", "boss", `{}`); rec.Code != 400 {
		t.Errorf("empty patch = %d, want 400", rec.Code)
	}
}

func deleteUserReq(t *testing.T, auth *AuthConfig, target, by string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("DELETE", "/api/users/"+target, nil)
	req.SetPathValue("username", target)
	if by != "" {
		req = req.WithContext(context.WithValue(req.Context(), userCtxKey{}, by))
	}
	rec := httptest.NewRecorder()
	handleUserDelete(auth)(rec, req)
	return rec
}

// Deleting is a registry removal, not a revocation — the response has to name
// what still grants access, or an admin will believe it did more than it did.
func TestDeleteUserReportsRemainingReferences(t *testing.T) {
	instanceStore(t, `{"theirs": {"owner": "bob"}, "shared": {"readers": ["bob"]}, "other": {"owner": "alice"}}`)
	userRegistry(t, `{"bob": {"source": "oauth"}, "boss": {"source": "oauth"}}`)
	auth := &AuthConfig{admins: map[string]bool{"boss": true}}

	rec := deleteUserReq(t, auth, "bob", "boss")
	if rec.Code != 200 {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body)
	}
	var got struct {
		Username          string
		StillReferencedBy []string `json:"stillReferencedBy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.StillReferencedBy, []string{"shared", "theirs"}) {
		t.Errorf("stillReferencedBy = %v, want [shared theirs]", got.StillReferencedBy)
	}
	users, _ := config.LoadUsers()
	if _, ok := users["bob"]; ok {
		t.Error("record survived the delete")
	}
	// The instances are deliberately left alone.
	insts, _ := config.LoadInstances()
	if insts["theirs"].Owner != "bob" {
		t.Errorf("delete rewrote instances.json: %+v", insts["theirs"])
	}
}

func TestDeleteRefusals(t *testing.T) {
	instanceStore(t, `{}`)
	userRegistry(t, `{"bob": {"source": "oauth"}, "boss": {"source": "oauth"}}`)
	auth := &AuthConfig{admins: map[string]bool{"boss": true}}

	if rec := deleteUserReq(t, auth, "bob", "bob"); rec.Code != 409 {
		t.Errorf("self-delete = %d, want 409", rec.Code)
	}
	if rec := deleteUserReq(t, auth, "boss", "bob"); rec.Code != 409 {
		t.Errorf("env-admin delete = %d, want 409", rec.Code)
	}
	if rec := deleteUserReq(t, auth, "nobody", "boss"); rec.Code != 400 {
		t.Errorf("unknown user = %d, want 400", rec.Code)
	}
	users, _ := config.LoadUsers()
	if len(users) != 2 {
		t.Errorf("registry changed despite refusals: %v", users)
	}
}
