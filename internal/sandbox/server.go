package sandbox

/*
Local Canvas look-alike for sandbox mode: answers /api/v1/... from the Store and serves downloaded files, so
every Canvas client in the app (tools, file downloader, course picker) works unchanged against it.

Reads: an exact captured response first (errors included, e.g. a hidden Files tab's 401); otherwise requests
whose parameters can't be pre-captured are answered from captured data (file search, calendar date ranges,
announcements for several courses); anything else is a 404 saying it wasn't captured.
Writes (POST/PUT/PATCH/DELETE): recorded in writes.jsonl and never sent anywhere. Submissions and discussion
replies also update the captured data, so later reads show them.
Every request is logged to requests.jsonl.

File links: no response ever points at Canvas. Every Canvas file link (file objects' download URLs, module items,
links inside page/assignment/announcement HTML) is rewritten to this platform as a signed, expiring link. With a
public URL configured, a separate files-only server serves those links (the API server stays on loopback);
without one the API server serves them itself.

The API only accepts the simulated access token the sandbox was started with; the real Canvas token is never
accepted, and this server never knows it.
*/

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dlclark/regexp2"

	"canvas_go/internal/jsonval"
	"canvas_go/internal/textutil"
)

var (
	submissionRE  = regexp.MustCompile(`^courses/(\d+)/assignments/(\d+)/submissions$`)
	entryRE       = regexp.MustCompile(`^courses/(\d+)/discussion_topics/(\d+)/entries$`)
	courseFilesRE = regexp.MustCompile(`^courses/(\d+)/files$`)
	filesPathRE   = regexp.MustCompile(`^/files/([^/]+)/([^/]+)$`)
)

func errorBody(message string) map[string]any {
	return map[string]any{"errors": []any{map[string]any{"message": message}}}
}

// FileLinks makes signed, expiring public download links: {public_url}/files/<id>/<name>?expires=<ts>&sig=<hmac>.
type FileLinks struct {
	PublicURL  string
	Secret     []byte
	TTLSeconds int64
}

func NewFileLinks(publicURL string, secret []byte, ttlSeconds int64) *FileLinks {
	return &FileLinks{PublicURL: strings.TrimRight(publicURL, "/"), Secret: secret, TTLSeconds: ttlSeconds}
}

func (f *FileLinks) sign(fileID string, expires int64) string {
	mac := hmac.New(sha256.New, f.Secret)
	fmt.Fprintf(mac, "%s:%d", fileID, expires)
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// Link is a fresh signed link for a file.
func (f *FileLinks) Link(fileID, name string) string {
	if name == "" {
		name = "file"
	}
	expires := time.Now().Unix() + f.TTLSeconds
	return fmt.Sprintf("%s/files/%s/%s?expires=%d&sig=%s", f.PublicURL, fileID, quoteAll(name), expires, f.sign(fileID, expires))
}

// Valid checks a link's signature and expiry.
func (f *FileLinks) Valid(fileID, expires, sig string) bool {
	if expires == "" || strings.Trim(expires, "0123456789") != "" {
		return false
	}
	ts, err := strconv.ParseInt(expires, 10, 64)
	if err != nil || float64(ts) < float64(time.Now().UnixNano())/1e9 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(f.sign(fileID, ts)), []byte(sig)) == 1
}

// quoteAll percent-encodes everything except RFC 3986 unreserved characters (so "/" is encoded too).
func quoteAll(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.IndexByte("_.-~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// Server is the local Canvas look-alike.
type Server struct {
	Store     *Store
	Token     string // the simulated access token; the real Canvas token is never accepted (or known) here
	FileLinks *FileLinks
	BaseURL   string

	mu                sync.Mutex // handlers mutate captured data
	servers           []*http.Server
	ids               atomic.Int64
	canvasFileLink    *regexp2.Regexp
	canvasStorageLink *regexp2.Regexp
}

func NewServer(store *Store, token string, links *FileLinks) *Server {
	s := &Server{Store: store, Token: token, FileLinks: links}
	s.ids.Store(time.Now().Unix())
	source, _ := store.Manifest["source"].(string)
	host := "canvas.invalid"
	if u, err := url.Parse(source); err == nil && u.Host != "" {
		host = u.Host
	}
	// Canvas file links in captured data, absolute (on the captured Canvas host) or relative:
	// /files/123, /courses/4/files/123/download?wrap=1, /api/v1/courses/4/files/123, ...
	s.canvasFileLink = regexp2.MustCompile(`(?<![\w.:/%-])(?:https?://`+regexp.QuoteMeta(host)+`)?/(?:api/v1/)?(?:(?:courses|users|groups)/\d+/)?`+
		`files/(\d+)(?:/(?:download|preview)?)?(?:\?[^\s"'<>)]*)?(?=[\s"'<>)]|$)`, regexp2.None)
	// Canvas's file storage links: the attachment id is the file id
	s.canvasStorageLink = regexp2.MustCompile(`https?://instructure-uploads[\w.-]*\.amazonaws\.com/[^\s"'<>)]*?/attachments/(\d+)/[^\s"'<>)]*`, regexp2.None)
	return s
}

// Start serves the API (loopback only: it holds all captured data and accepts writes). port 0 picks one.
func (s *Server) Start(host string, port int) (string, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/", s.api)
	mux.HandleFunc("/files/", s.publicDownload) // signed links when there's no public URL
	actual, err := s.serve(mux, host, port)
	if err != nil {
		return "", err
	}
	s.BaseURL = fmt.Sprintf("http://%s:%d", host, actual)
	return s.BaseURL, nil
}

// StartPublicFiles serves the files-only server behind the public URL: signed links in, file bytes out.
func (s *Server) StartPublicFiles(host string, port int) (int, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/files/", s.publicDownload)
	return s.serve(mux, host, port)
}

func (s *Server) serve(handler http.Handler, host string, port int) (int, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return 0, err
	}
	server := &http.Server{Handler: handler, MaxHeaderBytes: 1 << 20}
	go server.Serve(listener)
	s.mu.Lock()
	s.servers = append(s.servers, server)
	s.mu.Unlock()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

// Stop shuts every server down.
func (s *Server) Stop() {
	s.mu.Lock()
	servers := s.servers
	s.servers = nil
	s.mu.Unlock()
	for _, server := range servers {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		server.Shutdown(ctx)
		cancel()
	}
}

// orderedQuery parses a raw query keeping order and repeats.
func orderedQuery(raw string) []Param {
	var out []Param
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		key, err1 := url.QueryUnescape(k)
		value, err2 := url.QueryUnescape(v)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, Param{key, value})
	}
	return out
}

func paramsJSON(params []Param) []any {
	out := make([]any, len(params))
	for i, p := range params {
		out[i] = []any{p.Key, p.Value}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write([]byte(jsonval.JSON(body)))
}

func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/"), "/")
	params := orderedQuery(r.URL.RawQuery)
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.Token)) != 1 {
		s.Store.LogRequest(map[string]any{"method": r.Method, "path": path, "query": paramsJSON(params),
			"outcome": "unauthorized", "status": 401})
		writeJSON(w, 401, map[string]any{"status": "unauthenticated", "errors": []any{map[string]any{"message": "Invalid access token."}}})
		return
	}
	s.mu.Lock()
	var outcome string
	var status int
	var body any
	if r.Method == http.MethodGet {
		outcome, status, body = s.read(path, params)
	} else {
		payload := readPayload(r)
		outcome, status, body = "write", 200, s.write(r.Method, path, params, payload)
	}
	localized := s.localize(body)
	s.mu.Unlock()
	s.Store.LogRequest(map[string]any{"method": r.Method, "path": path, "query": paramsJSON(params),
		"outcome": outcome, "status": status})
	writeJSON(w, status, localized)
}

func (s *Server) publicDownload(w http.ResponseWriter, r *http.Request) {
	m := filesPathRE.FindStringSubmatch(r.URL.Path)
	if m == nil || r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	fileID := m[1]
	query := r.URL.Query()
	ok := s.FileLinks != nil && s.FileLinks.Valid(fileID, query.Get("expires"), query.Get("sig"))
	path := ""
	if ok {
		path = s.Store.FilePath(fileID)
	}
	status, outcome := 200, "file"
	if path == "" {
		if ok {
			status, outcome = 404, "miss"
		} else {
			status, outcome = 403, "bad-link"
		}
	}
	s.Store.LogRequest(map[string]any{"method": "GET", "path": "public/files/" + fileID, "outcome": outcome, "status": status})
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if !ok {
		w.WriteHeader(403)
		io.WriteString(w, "This download link is invalid or has expired. Ask the assistant again for a fresh link.")
		return
	}
	if path == "" {
		w.WriteHeader(404)
		io.WriteString(w, "This file isn't available: Canvas didn't allow it to be downloaded.")
		return
	}
	name := filepath.Base(path)
	s.Store.mu.Lock()
	if entry := s.Store.Files[fileID]; entry != nil {
		if display, ok := entry.DisplayName.(string); ok && display != "" {
			name = display
		}
	}
	s.Store.mu.Unlock()
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, _ := f.Stat()
	w.Header().Del("Content-Type")
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+quoteAll(name))
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// --- reads ---

func (s *Server) read(path string, params []Param) (string, int, any) {
	if entry := s.Store.Get(RequestKey("GET", path, params)); entry != nil {
		return "hit", entry.Status, entry.Body
	}
	// A filtered request on an endpoint Canvas refused (e.g. searching a hidden Files tab): same refusal
	for _, p := range params {
		if p.Key == "search_term" {
			if base := s.Store.Get(RequestKey("GET", path, nil)); base != nil && base.Status != 200 {
				return "hit", base.Status, base.Body
			}
			break
		}
	}
	if emulated, ok := s.emulate(path, params); ok {
		return "emulated", 200, emulated
	}
	return "miss", 404, errorBody(fmt.Sprintf(
		"Not captured in the Canvas sandbox: GET %s. Run `canvas_sandbox sync` to refresh it.", path))
}

func (s *Server) emulate(path string, params []Param) (any, bool) {
	values := map[string][]string{}
	for _, p := range params {
		values[p.Key] = append(values[p.Key], p.Value)
	}
	first := func(key, def string) string {
		if v := values[key]; len(v) > 0 {
			return v[0]
		}
		return def
	}

	search := strings.ToLower(strings.TrimSpace(first("search_term", "")))
	if search != "" && courseFilesRE.MatchString(path) {
		if files, ok := s.Store.Body(RequestKey("GET", path, nil)).([]any); ok {
			out := []any{}
			for _, f := range files {
				m, _ := f.(map[string]any)
				if strings.Contains(strings.ToLower(strField(m, "display_name")), search) ||
					strings.Contains(strings.ToLower(strField(m, "filename")), search) {
					out = append(out, f)
				}
			}
			return out, true
		}
	}

	if path == "announcements" && len(values["context_codes[]"]) > 0 {
		merged, found := []any{}, false
		for _, code := range values["context_codes[]"] {
			for _, part := range strings.Split(code, ",") {
				key := RequestKey("GET", "announcements", []Param{{"context_codes[]", strings.TrimSpace(part)}})
				if items, ok := s.Store.Body(key).([]any); ok {
					found = true
					merged = append(merged, items...)
				}
			}
		}
		if found {
			sort.SliceStable(merged, func(i, j int) bool {
				return postedAt(merged[i]) > postedAt(merged[j])
			})
			return merged, true
		}
	}

	if path == "calendar_events" && (len(values["start_date"]) > 0 || len(values["end_date"]) > 0) {
		if events, ok := s.Store.Body(RequestKey("GET", "calendar_events", []Param{{"all_events", "true"}})).([]any); ok {
			start := textutil.Substr(first("start_date", ""), 0, 10)
			end := textutil.Substr(first("end_date", "9999-12-31"), 0, 10)
			out := []any{}
			for _, e := range events {
				m, _ := e.(map[string]any)
				day := textutil.Substr(stringOrEmpty(m["start_at"]), 0, 10)
				if start <= day && day <= end {
					out = append(out, e)
				}
			}
			return out, true
		}
	}
	return nil, false
}

func strField(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	return jsonval.Text(v)
}

func stringOrEmpty(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func postedAt(v any) string {
	m, _ := v.(map[string]any)
	return stringOrEmpty(m["posted_at"])
}

// --- writes ---

func readPayload(r *http.Request) any {
	data, err := io.ReadAll(io.LimitReader(r.Body, 64*1024*1024))
	if err != nil || len(data) == 0 {
		return nil
	}
	if v, err := jsonval.Decode(data); err == nil {
		return v
	}
	if form, err := url.ParseQuery(string(data)); err == nil && len(form) > 0 {
		out := map[string]any{}
		for k, v := range form {
			out[k] = v[len(v)-1]
		}
		return out
	}
	return map[string]any{"raw": textutil.Substr(textutil.DecodeUTF8(data), 0, 10000)}
}

func (s *Server) write(method, path string, params []Param, payload any) map[string]any {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00")
	note := "Recorded in the local Canvas sandbox; nothing was sent to Canvas."
	body, _ := payload.(map[string]any)
	var result map[string]any
	if m := submissionRE.FindStringSubmatch(path); method == http.MethodPost && m != nil {
		courseID, assignmentID := m[1], m[2]
		submission, _ := body["submission"].(map[string]any)
		aid, _ := strconv.ParseInt(assignmentID, 10, 64)
		cid, _ := strconv.ParseInt(courseID, 10, 64)
		result = map[string]any{
			"id": fmt.Sprintf("sandbox-%d", s.nextID()), "assignment_id": aid, "course_id": cid,
			"workflow_state": "submitted", "submitted_at": now,
			"submission_type": submission["submission_type"],
			"body":            submission["body"], "url": submission["url"],
			"sandbox": true, "note": note,
		}
		s.applySubmission(courseID, assignmentID, result)
	} else if m := entryRE.FindStringSubmatch(path); method == http.MethodPost && m != nil {
		courseID, topicID := m[1], m[2]
		result = map[string]any{
			"id": fmt.Sprintf("sandbox-%d", s.nextID()), "message": body["message"], "created_at": now,
			"user_id": s.Store.Manifest["user_id"], "sandbox": true, "note": note,
		}
		s.applyEntry(courseID, topicID, result)
	} else {
		result = map[string]any{"sandbox": true, "recorded": true, "method": method, "path": path, "note": note}
	}
	s.Store.RecordWrite(map[string]any{"method": method, "path": path, "query": paramsJSON(params), "body": payload, "response": result})
	return result
}

func (s *Server) nextID() int64 { return s.ids.Add(1) - 1 }

func (s *Server) applySubmission(courseID, assignmentID string, submission map[string]any) {
	assignments, ok := s.Store.Body(RequestKey("GET", "courses/"+courseID+"/assignments", nil)).([]any)
	if !ok {
		return
	}
	for _, a := range assignments {
		assignment, _ := a.(map[string]any)
		if assignment == nil || jsonval.Text(assignment["id"]) != assignmentID {
			continue
		}
		merged := map[string]any{}
		if existing, ok := assignment["submission"].(map[string]any); ok {
			for k, v := range existing {
				merged[k] = v
			}
		}
		for k, v := range submission {
			merged[k] = v
		}
		assignment["submission"] = merged
		assignment["has_submitted_submissions"] = true
	}
	s.Store.Save()
}

func (s *Server) applyEntry(courseID, topicID string, entry map[string]any) {
	entriesKey := RequestKey("GET", "courses/"+courseID+"/discussion_topics/"+topicID+"/entries", nil)
	entries, ok := s.Store.Body(entriesKey).([]any)
	if !ok {
		entries = []any{}
	}
	s.Store.Put(entriesKey, 200, append(entries, entry))
	if topics, ok := s.Store.Body(RequestKey("GET", "courses/"+courseID+"/discussion_topics", nil)).([]any); ok {
		for _, t := range topics {
			topic, _ := t.(map[string]any)
			if topic != nil && jsonval.Text(topic["id"]) == topicID {
				count, _ := jsonval.Float(topic["discussion_subentry_count"])
				topic["discussion_subentry_count"] = int64(count) + 1
			}
		}
	}
	s.Store.Save()
}

// --- responses ---

// FileLink is a signed download link for a file: on the public URL if configured, else on this server.
func (s *Server) FileLink(fileID any, name *string) string {
	id := jsonval.Text(fileID)
	display := ""
	if name != nil {
		display = *name
	} else {
		s.Store.mu.Lock()
		entry := s.Store.Files[id]
		s.Store.mu.Unlock()
		if entry != nil {
			display, _ = entry.DisplayName.(string)
		}
		if display == "" {
			if meta, ok := s.Store.Body(RequestKey("GET", "files/"+id, nil)).(map[string]any); ok {
				display, _ = meta["display_name"].(string)
			}
		}
	}
	return s.FileLinks.Link(id, display)
}

// localize copies body so that no file link points at Canvas: each one is rewritten to this platform.
func (s *Server) localize(body any) any {
	switch x := body.(type) {
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = s.localize(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[k] = s.localize(v)
		}
		_, hasID := out["id"]
		_, hasURL := out["url"]
		_, hasDisplay := out["display_name"]
		_, hasFilename := out["filename"]
		if hasID && hasURL && (hasDisplay || hasFilename) {
			name := ""
			if jsonval.Truthy(out["display_name"]) {
				name = jsonval.Text(out["display_name"])
			} else if jsonval.Truthy(out["filename"]) {
				name = jsonval.Text(out["filename"])
			}
			if name == "" {
				out["url"] = s.FileLink(out["id"], nil)
			} else {
				out["url"] = s.FileLink(out["id"], &name)
			}
		}
		return out
	case string:
		text := x
		if strings.Contains(text, "instructure-uploads") {
			if replaced, err := s.canvasStorageLink.ReplaceFunc(text, func(m regexp2.Match) string {
				return s.FileLink(m.GroupByNumber(1).String(), nil)
			}, -1, -1); err == nil {
				text = replaced
			}
		}
		if strings.Contains(text, "files/") {
			if replaced, err := s.canvasFileLink.ReplaceFunc(text, func(m regexp2.Match) string {
				return s.FileLink(m.GroupByNumber(1).String(), nil)
			}, -1, -1); err == nil {
				text = replaced
			}
		}
		return text
	}
	return body
}
