package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// User is a known identity: someone who has signed in, or who an admin added
// ahead of time so they can be granted instance access before first login.
type User struct {
	Email     string    `json:"email,omitempty"`
	Source    string    `json:"source"`             // "oauth", "password" or "manual"
	AddedBy   string    `json:"added_by,omitempty"` // for manually added users
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen,omitempty"`
	// Admin grants this user admin rights on top of EC2CP_ADMINS. Granted by
	// another admin through the UI; the env list stays the bootstrap set and
	// cannot be revoked here.
	Admin   bool   `json:"admin,omitempty"`
	AdminBy string `json:"admin_by,omitempty"` // who granted it — audit
	AdminAt string `json:"admin_at,omitempty"` // RFC3339, audit
}

// Users maps username (the email local-part) to its record.
type Users map[string]User

var usersMu sync.Mutex

// UsersPath exposes the registry location so callers can watch it for changes.
func UsersPath() string { return usersPath() }

// usersPath is the registry location. It lives in the state directory so it
// persists across container recreation (a single-file bind mount would not).
func usersPath() string {
	if p := os.Getenv("EC2CP_USER_DB"); p != "" {
		return p
	}
	return filepath.Join("state", "users.json")
}

// LoadUsers reads the registry. A missing file is an empty registry, not an error.
func LoadUsers() (Users, error) {
	data, err := os.ReadFile(usersPath())
	if os.IsNotExist(err) {
		return Users{}, nil
	}
	if err != nil {
		return nil, err
	}
	users := Users{}
	if err := json.Unmarshal(data, &users); err != nil {
		return nil, fmt.Errorf("%s: %w", usersPath(), err)
	}
	return users, nil
}

// Usernames returns the known usernames, sorted.
func Usernames(u Users) []string {
	out := make([]string, 0, len(u))
	for name := range u {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// SetUserAdmin grants or revokes the registry admin flag. The user must already
// be known: the UI grants from the list, and inventing a record here would let
// a typo create a phantom admin. Revoking is silent about EC2CP_ADMINS — the
// caller checks that, because this package cannot see the env list.
func SetUserAdmin(username string, admin bool, by string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return fmt.Errorf("username is required")
	}
	usersMu.Lock()
	defer usersMu.Unlock()

	users, err := LoadUsers()
	if err != nil {
		return err // unlike a sign-in, this must not silently start from empty
	}
	rec, ok := users[username]
	if !ok {
		return fmt.Errorf("unknown user %q — register them first", username)
	}
	rec.Admin = admin
	if admin {
		rec.AdminBy, rec.AdminAt = by, time.Now().UTC().Format(time.RFC3339)
	} else {
		rec.AdminBy, rec.AdminAt = "", ""
	}
	users[username] = rec
	return writeUsers(users)
}

// AdminUsernames returns the users carrying the registry admin flag, sorted.
func AdminUsernames(u Users) []string {
	var out []string
	for name, rec := range u {
		if rec.Admin {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// RecordUser upserts a user. Sign-ins refresh LastSeen without disturbing how
// the user was first registered; a manual add never downgrades an existing
// record's source.
func RecordUser(username, email, source, addedBy string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return fmt.Errorf("username is required")
	}
	usersMu.Lock()
	defer usersMu.Unlock()

	users, err := LoadUsers()
	if err != nil {
		users = Users{} // a corrupt registry must not block sign-in
	}
	now := time.Now().UTC()
	rec, existed := users[username]
	if !existed {
		rec = User{Source: source, AddedBy: addedBy, FirstSeen: now}
	}
	if email != "" {
		rec.Email = email
	}
	if source != "manual" {
		rec.LastSeen = now // an actual sign-in
		if rec.Source == "manual" {
			rec.Source = source // they've now really signed in
		}
	}
	users[username] = rec
	return writeUsers(users)
}

// SetUserEmail records a contact address. Empty clears it.
func SetUserEmail(username, email string) error {
	return mutateUser(username, func(rec *User) { rec.Email = strings.ToLower(strings.TrimSpace(email)) })
}

// mutateUser applies APPLY to an existing record and persists the registry.
func mutateUser(username string, apply func(*User)) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return fmt.Errorf("username is required")
	}
	usersMu.Lock()
	defer usersMu.Unlock()

	users, err := LoadUsers()
	if err != nil {
		return err
	}
	rec, ok := users[username]
	if !ok {
		return fmt.Errorf("unknown user %q", username)
	}
	apply(&rec)
	users[username] = rec
	return writeUsers(users)
}

// DeleteUser drops the registry record. It does NOT revoke access: an OAuth
// sign-in re-registers the name, and instances.json may still list it. Callers
// should surface UserReferences first so the deletion is not mistaken for a
// revocation.
func DeleteUser(username string) error {
	username = strings.TrimSpace(username)
	usersMu.Lock()
	defer usersMu.Unlock()

	users, err := LoadUsers()
	if err != nil {
		return err
	}
	if _, ok := users[username]; !ok {
		return fmt.Errorf("unknown user %q", username)
	}
	delete(users, username)
	return writeUsers(users)
}

// RenameUser moves a registry record to a new username, carrying its ACL
// references with it. instances.json is rewritten FIRST: usernames are the only
// thing tying an instance to its owner, so a crash between the two writes must
// leave the access intact rather than orphaned. Returns how many instances were
// touched.
func RenameUser(oldName, newName string) (int, error) {
	oldName, newName = strings.TrimSpace(oldName), strings.TrimSpace(newName)
	if oldName == "" || newName == "" {
		return 0, fmt.Errorf("both names are required")
	}
	if oldName == newName {
		return 0, nil
	}

	usersMu.Lock()
	defer usersMu.Unlock()
	users, err := LoadUsers()
	if err != nil {
		return 0, err
	}
	rec, ok := users[oldName]
	if !ok {
		return 0, fmt.Errorf("unknown user %q", oldName)
	}
	if _, taken := users[newName]; taken {
		return 0, fmt.Errorf("user %q already exists", newName)
	}

	touched, err := renameInstanceReferences(oldName, newName)
	if err != nil {
		return 0, err
	}

	delete(users, oldName)
	users[newName] = rec
	if err := writeUsers(users); err != nil {
		return touched, err
	}
	return touched, nil
}

// renameInstanceReferences rewrites owner and readers entries naming OLD.
func renameInstanceReferences(old, new string) (int, error) {
	instancesMu.Lock()
	defer instancesMu.Unlock()
	path, err := resolveInstancesPath()
	if err != nil {
		return 0, err
	}
	insts, err := loadInstancesFrom(path)
	if err != nil {
		return 0, err
	}
	touched := 0
	for id, cfg := range insts {
		changed := false
		if cfg.Owner == old {
			cfg.Owner, changed = new, true
		}
		for i, r := range cfg.Readers {
			if r == old {
				cfg.Readers[i], changed = new, true
			}
		}
		if changed {
			insts[id] = cfg
			touched++
		}
	}
	if touched == 0 {
		return 0, nil
	}
	return touched, writeInstances(path, insts)
}

// UserReferences lists the instances naming USERNAME as owner or reader, so a
// delete can say what will be left pointing at a name the registry forgot.
func UserReferences(username string) ([]string, error) {
	insts, err := LoadInstances()
	if err != nil {
		return nil, err
	}
	var out []string
	for id, cfg := range insts {
		if cfg.Owner == username || slices.Contains(cfg.Readers, username) {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}

// writeUsers persists the registry. Callers hold usersMu.
func writeUsers(users Users) error {
	data, err := json.MarshalIndent(users, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(usersPath(), append(data, '\n'))
}
