package sandbox

/*
On-disk Canvas sandbox: every captured API response, the downloaded course files, and the writes the agent
made while in sandbox mode (recorded here, never sent to Canvas).

Layout under the sandbox directory:

	manifest.json     when it was captured, from which host, counts and per-endpoint errors
	responses.json    {request key: {"status": int, "body": <JSON>}}
	files.json        {file id: {"path": <relative path>, "size", "content_type", "display_name"}}
	files/<id>/<name> the downloaded file contents
	writes.jsonl      one line per write (submission, discussion post, ...) made in sandbox mode
	requests.jsonl    one line per API request served in sandbox mode, and how it was answered
*/

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"canvas_go/internal/jsonval"
)

// IgnoredParams never change which data a request returns from the sandbox: paging (the sandbox stores
// complete lists) and include[] (everything is captured with every include).
var IgnoredParams = map[string]bool{"per_page": true, "page": true, "include[]": true, "access_token": true}

// Param is one query parameter (order and repeats matter, like a multidict).
type Param struct {
	Key   string
	Value string
}

// RequestKey is the canonical key for a Canvas API request, e.g. "GET courses/12/files?search_term=lab".
func RequestKey(method, path string, params []Param) string {
	path = strings.Trim(path, "/")
	path = strings.TrimPrefix(path, "api/v1/")
	var kept []Param
	for _, p := range params {
		if !IgnoredParams[p.Key] {
			kept = append(kept, p)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].Key != kept[j].Key {
			return kept[i].Key < kept[j].Key
		}
		return kept[i].Value < kept[j].Value
	})
	parts := make([]string, len(kept))
	for i, p := range kept {
		parts[i] = p.Key + "=" + p.Value
	}
	key := strings.ToUpper(method) + " " + path
	if len(parts) > 0 {
		key += "?" + strings.Join(parts, "&")
	}
	return key
}

// Response is a stored API answer.
type Response struct {
	Status int `json:"status"`
	Body   any `json:"body"`
}

// FileEntry is a downloaded file.
type FileEntry struct {
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	ContentType any    `json:"content_type"`
	DisplayName any    `json:"display_name"`
}

// Store is a sandbox on disk. Bodies are decoded JSON (numbers as json.Number).
type Store struct {
	Root      string
	mu        sync.Mutex
	Responses map[string]*Response
	Files     map[string]*FileEntry
	Manifest  map[string]any
}

func NewStore(root string) *Store {
	return &Store{Root: root, Responses: map[string]*Response{}, Files: map[string]*FileEntry{}, Manifest: map[string]any{}}
}

// IsComplete is true once a sync has finished writing this sandbox.
func (s *Store) IsComplete() bool {
	data, err := os.ReadFile(filepath.Join(s.Root, "manifest.json"))
	if err != nil {
		return false
	}
	var manifest map[string]any
	if json.Unmarshal(data, &manifest) != nil {
		return false
	}
	return manifest["complete"] == true
}

func readJSON(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	return dec.Decode(out)
}

// Load reads the sandbox from disk.
func (s *Store) Load() (*Store, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	manifest := map[string]any{}
	if err := readJSON(filepath.Join(s.Root, "manifest.json"), &manifest); err != nil {
		return nil, err
	}
	responses := map[string]*Response{}
	if err := readJSON(filepath.Join(s.Root, "responses.json"), &responses); err != nil {
		return nil, err
	}
	files := map[string]*FileEntry{}
	if _, err := os.Stat(filepath.Join(s.Root, "files.json")); err == nil {
		if err := readJSON(filepath.Join(s.Root, "files.json"), &files); err != nil {
			return nil, err
		}
	}
	s.Manifest, s.Responses, s.Files = manifest, responses, files
	return s, nil
}

func atomicWriteJSON(path string, data any) error {
	out, err := jsonval.Marshal(data)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Save writes responses, files and the manifest.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.Root, 0o755); err != nil {
		return err
	}
	if err := atomicWriteJSON(filepath.Join(s.Root, "responses.json"), s.Responses); err != nil {
		return err
	}
	if err := atomicWriteJSON(filepath.Join(s.Root, "files.json"), s.Files); err != nil {
		return err
	}
	return atomicWriteJSON(filepath.Join(s.Root, "manifest.json"), s.Manifest)
}

// Put stores a response.
func (s *Store) Put(key string, status int, body any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Responses[key] = &Response{Status: status, Body: body}
}

// Get returns a stored response, or nil.
func (s *Store) Get(key string) *Response {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Responses[key]
}

// Body is the stored body of a successful response, else nil.
func (s *Store) Body(key string) any {
	entry := s.Get(key)
	if entry != nil && entry.Status == 200 {
		return entry.Body
	}
	return nil
}

// FilePath is the downloaded copy of a file, or "" when there is none.
func (s *Store) FilePath(fileID string) string {
	s.mu.Lock()
	entry := s.Files[fileID]
	s.mu.Unlock()
	if entry == nil {
		return ""
	}
	path := filepath.Join(s.Root, entry.Path)
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return path
}

func (s *Store) appendLine(name string, entry map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	os.MkdirAll(s.Root, 0o755)
	f, err := os.OpenFile(filepath.Join(s.Root, name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	line := map[string]any{"ts": float64(time.Now().UnixNano()) / 1e9}
	for k, v := range entry {
		line[k] = v
	}
	f.WriteString(jsonval.JSON(line) + "\n")
}

// LogRequest records an API request served in sandbox mode.
func (s *Store) LogRequest(entry map[string]any) { s.appendLine("requests.jsonl", entry) }

// RecordWrite records a write made in sandbox mode.
func (s *Store) RecordWrite(entry map[string]any) { s.appendLine("writes.jsonl", entry) }
