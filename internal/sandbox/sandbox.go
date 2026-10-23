// Package sandbox is Canvas sandbox mode (CANVAS_SANDBOX=yes in .env).
//
// On start, if no complete sandbox exists yet (or CANVAS_SANDBOX_REFRESH=yes), everything the Canvas tools
// could ask for is fetched from Canvas with the access token and every file is downloaded (see sync.go). Then a
// local Canvas look-alike server starts on 127.0.0.1 and CANVAS_URL is pointed at it, so every tool call, file
// download and course listing is answered locally, and every write (assignment submission, discussion reply,
// ...) is recorded in the sandbox instead of reaching Canvas.
//
// Credentials: the capture is the ONLY step that uses the real CANVAS_URL and CANVAS_ACCESS_TOKEN. Once the
// sandbox starts, both are replaced in this process by the local server's URL and a simulated token that only
// the local server accepts, so nothing after the capture holds the real ones. A process that serves an
// already-captured sandbox doesn't need the real credentials at all.
//
// Settings:
//
//	CANVAS_SANDBOX=yes|no            turn sandbox mode on
//	CANVAS_SANDBOX_DIR=<path>        where the sandbox lives (default: workdir/canvas_sandbox)
//	CANVAS_SANDBOX_REFRESH=yes|no    re-fetch everything from Canvas on the next start
//	CANVAS_SANDBOX_PUBLIC_URL=<url>  public base URL file links should use (e.g. https://canvas.marksong.tech);
//	                                 its /files/ path must reach the files-only server below
//	CANVAS_SANDBOX_FILES_HOST/PORT   where the files-only server listens (default 127.0.0.1:8766)
//	CANVAS_SANDBOX_LINK_TTL_HOURS    how long a file link stays valid (default 168 = 7 days)
//	CANVAS_SANDBOX_LINK_SECRET       key that signs file links (default: a random key kept in the sandbox dir)
//
// Manual commands: `canvas_sandbox sync` (capture/refresh now) and `canvas_sandbox status`.
package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"canvas_go/internal/env"
)

var (
	serverMu sync.Mutex
	server   *Server
)

// Enabled reports whether sandbox mode is on.
func Enabled() bool { return env.True(env.Get("CANVAS_SANDBOX", "no")) }

// Dir is where the sandbox lives.
func Dir() string {
	if dir := os.Getenv("CANVAS_SANDBOX_DIR"); dir != "" {
		return dir
	}
	root := os.Getenv("CANVAS_GO_ROOT")
	if root == "" {
		root, _ = os.Getwd()
	}
	return filepath.Join(root, "workdir", "canvas_sandbox")
}

// ActiveURL is the URL of the local sandbox server while it runs in this process, else "".
func ActiveURL() string {
	serverMu.Lock()
	defer serverMu.Unlock()
	if server == nil {
		return ""
	}
	return server.BaseURL
}

// LocalFilePath is the downloaded copy of a file in the running sandbox, or "".
func LocalFilePath(fileID string) string {
	serverMu.Lock()
	s := server
	serverMu.Unlock()
	if s == nil {
		return ""
	}
	return s.Store.FilePath(fileID)
}

// Running returns the running server (nil when sandbox mode isn't running).
func Running() *Server {
	serverMu.Lock()
	defer serverMu.Unlock()
	return server
}

// StopRunning stops the running sandbox server (used by tests and restarts).
func StopRunning() {
	serverMu.Lock()
	s := server
	server = nil
	serverMu.Unlock()
	if s != nil {
		s.Stop()
	}
}

func tokenHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// linkSecret signs file links: CANVAS_SANDBOX_LINK_SECRET, else a random key kept in the sandbox (not the token).
func linkSecret(root string) ([]byte, error) {
	if secret := os.Getenv("CANVAS_SANDBOX_LINK_SECRET"); secret != "" {
		return []byte(secret), nil
	}
	path := filepath.Join(root, "link_secret")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(path, []byte(tokenHex(32)), 0o600); err != nil {
			return nil, err
		}
		os.Chmod(path, 0o600)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return []byte(strings.TrimSpace(string(data))), nil
}

// Start runs sandbox mode: capture Canvas first if needed, start the local server, then switch this process to
// simulated credentials (local URL + a token only the local server accepts). Returns the local server URL, or
// "" when sandbox mode is off. Safe to call more than once.
func Start(ctx context.Context, log func(string)) (string, error) {
	if log == nil {
		log = func(m string) { fmt.Println(m) }
	}
	if !Enabled() {
		return "", nil
	}
	serverMu.Lock()
	defer serverMu.Unlock()
	if server != nil {
		return server.BaseURL, nil
	}

	store := NewStore(Dir())
	if env.True(env.Get("CANVAS_SANDBOX_REFRESH", "no")) || !store.IsComplete() {
		// The capture is the only step that uses the real Canvas URL and access token
		realURL, realToken := os.Getenv("CANVAS_URL"), os.Getenv("CANVAS_ACCESS_TOKEN")
		if realURL == "" || realToken == "" || strings.HasPrefix(realToken, "sandbox-") {
			return "", errors.New("No complete Canvas sandbox yet: capture it first with the real CANVAS_URL and " +
				"CANVAS_ACCESS_TOKEN (canvas_sandbox sync).")
		}
		log(fmt.Sprintf("Canvas sandbox: capturing everything from %s into %s (this can take a while)...", realURL, store.Root))
		if _, err := Sync(ctx, store, realURL, realToken, log); err != nil {
			return "", err
		}
	}
	if _, err := store.Load(); err != nil {
		return "", err
	}

	simulatedToken := "sandbox-" + tokenHex(16)
	s := NewServer(store, simulatedToken, nil)
	url, err := s.Start("127.0.0.1", 0)
	if err != nil {
		return "", err
	}

	publicURL := strings.TrimSpace(os.Getenv("CANVAS_SANDBOX_PUBLIC_URL"))
	ttlHours, err := strconv.ParseFloat(env.Get("CANVAS_SANDBOX_LINK_TTL_HOURS", "168"), 64)
	if err != nil {
		s.Stop()
		return "", fmt.Errorf("CANVAS_SANDBOX_LINK_TTL_HOURS: %w", err)
	}
	secret, err := linkSecret(store.Root)
	if err != nil {
		s.Stop()
		return "", err
	}
	// Downloads are always signed links: on the public URL if set, else on the local server itself
	linkBase := publicURL
	if linkBase == "" {
		linkBase = url
	}
	s.FileLinks = NewFileLinks(linkBase, secret, int64(ttlHours*3600))
	if publicURL != "" {
		filesHost := env.Get("CANVAS_SANDBOX_FILES_HOST", "127.0.0.1")
		filesPort, err := strconv.Atoi(env.Get("CANVAS_SANDBOX_FILES_PORT", "8766"))
		if err != nil {
			s.Stop()
			return "", fmt.Errorf("CANVAS_SANDBOX_FILES_PORT: %w", err)
		}
		actual, err := s.StartPublicFiles(filesHost, filesPort)
		if err != nil {
			s.Stop()
			return "", err
		}
		log(fmt.Sprintf("Canvas sandbox: file links point at %s/files/... (served on %s:%d)", publicURL, filesHost, actual))
	}

	// From here on nothing in this process holds the real Canvas URL or token
	os.Setenv("CANVAS_URL", url)
	os.Setenv("CANVAS_ACCESS_TOKEN", simulatedToken)
	created := store.Manifest["created"]
	if created == nil {
		created = "?"
	}
	log(fmt.Sprintf("Canvas sandbox: serving %d captured responses and %d files (captured %v) at %s with simulated "+
		"credentials. Writes are recorded locally, never sent to Canvas.", len(store.Responses), len(store.Files), created, url))
	server = s
	return url, nil
}
