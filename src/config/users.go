package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
}

// Users maps username (the email local-part) to its record.
type Users map[string]User

var usersMu sync.Mutex

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

	data, err := json.MarshalIndent(users, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return WriteFileAtomic(usersPath(), data)
}
