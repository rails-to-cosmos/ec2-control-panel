package main

import (
	"os"
	"path/filepath"

	"github.com/joho/godotenv"
)

// loadDotenv searches CWD and ancestors for a .env file and loads it without
// overriding env vars already set in the process environment. Silent if not found.
func loadDotenv() {
	path := findDotenv()
	if path == "" {
		return
	}
	_ = godotenv.Load(path)
}

func findDotenv() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		p := filepath.Join(dir, ".env")
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
