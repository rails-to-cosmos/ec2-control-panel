package config

import (
	"github.com/joho/godotenv"
)

// LoadDotenv searches CWD and ancestors for a .env file and loads it without
// overriding env vars already set in the process environment. Silent if not found.
func LoadDotenv() {
	path := findDotenv()
	if path == "" {
		return
	}
	_ = godotenv.Load(path)
}

func findDotenv() string {
	p, _ := findUpwards(".env")
	return p
}
