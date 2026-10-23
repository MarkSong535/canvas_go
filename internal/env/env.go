// Package env loads a .env file into the process environment (variables that are already set are not
// overridden) and reads typed settings.
package env

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"
)

// Load reads the nearest .env (the working directory or one of its parents). Missing files are ignored.
func Load() {
	if path := Find(); path != "" {
		LoadFile(path)
	}
}

// Find returns the path of the nearest .env walking up from the working directory, or "".
func Find() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(dir, ".env")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// LoadFile sets the variables from a .env file that aren't set yet.
func LoadFile(path string) error { return godotenv.Load(path) }

// Get returns the variable, or def when it isn't set.
func Get(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

// True reports whether a yes/no setting is on ("1", "true", "yes", "on").
func True(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
