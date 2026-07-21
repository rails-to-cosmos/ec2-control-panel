package server

import (
	"testing"
	"time"
)

func TestSignUnsignRoundTrip(t *testing.T) {
	key := []byte("secret-key")
	tok := sign(key, map[string]any{"user": "alice", "exp": time.Now().Add(time.Minute).Unix()})
	data, ok := unsign(key, tok)
	if !ok || data["user"] != "alice" {
		t.Fatalf("roundtrip failed: ok=%v data=%v", ok, data)
	}
}

func TestUnsignRejectsTamperAndExpiry(t *testing.T) {
	key := []byte("secret-key")
	if _, ok := unsign(key, "tampered.signature"); ok {
		t.Fatal("tampered token accepted")
	}
	if _, ok := unsign([]byte("other-key"), sign(key, map[string]any{"user": "x"})); ok {
		t.Fatal("wrong-key token accepted")
	}
	expired := sign(key, map[string]any{"user": "x", "exp": time.Now().Add(-time.Second).Unix()})
	if _, ok := unsign(key, expired); ok {
		t.Fatal("expired token accepted")
	}
}

func TestHashVerifyPassword(t *testing.T) {
	h := HashPassword("hunter2")
	if !verifyPassword("hunter2", h) {
		t.Fatal("correct password rejected")
	}
	if verifyPassword("wrong", h) {
		t.Fatal("wrong password accepted")
	}
	if verifyPassword("x", "not-a-hash") {
		t.Fatal("malformed hash accepted")
	}
}

func TestParseUsers(t *testing.T) {
	got := parseUsers("alice:pbkdf2_sha256$1$a$b , bob:pbkdf2_sha256$1$c$d")
	if got["alice"] != "pbkdf2_sha256$1$a$b" || got["bob"] != "pbkdf2_sha256$1$c$d" {
		t.Fatalf("parseUsers wrong: %v", got)
	}
	if len(parseUsers("garbage-no-colon")) != 0 || len(parseUsers("")) != 0 {
		t.Fatal("expected empty map for malformed/empty input")
	}
}

func TestSafeNext(t *testing.T) {
	a := &AuthConfig{basePath: "/ec2"}
	if a.safeNext("/api/x") != "/api/x" {
		t.Fatal("local path should pass through")
	}
	if a.safeNext("//evil.com") != "/ec2/" || a.safeNext("https://evil.com") != "/ec2/" {
		t.Fatal("open-redirect not blocked to base path")
	}
}
