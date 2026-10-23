package tests

// Canvas sandbox tests against a simulated Canvas server: capture (pagination, a hidden Files tab, linked files,
// downloads), then the real tools served from the sandbox, with writes recorded locally and nothing reaching
// Canvas.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"canvas_go/internal/canvastools"
	"canvas_go/internal/config"
	"canvas_go/internal/sandbox"
	"canvas_go/internal/tools"
)

const simulatedCanvasToken = "Bearer test-token"

// SimulatedCanvas is just enough of the Canvas API for one student with two courses; it counts every request.
type SimulatedCanvas struct {
	server   *httptest.Server
	Base     string
	mu       sync.Mutex
	hits     []string
	contents map[int][]byte
}

func NewSimulatedCanvas() *SimulatedCanvas {
	c := &SimulatedCanvas{contents: map[int][]byte{
		94: makeDOCX([]string{"Master the Active Voice Exercise", "1. The essay was written by Maria.", "2. Mistakes were made."}, nil),
		93: []byte(strings.Repeat("Lecture transcript. ", 1000)), // 20,000 characters: read in parts
	}}
	c.server = httptest.NewServer(http.HandlerFunc(c.serve))
	c.Base = c.server.URL
	return c
}

func (c *SimulatedCanvas) Close() { c.server.Close() }

func (c *SimulatedCanvas) Hits() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.hits...)
}

func (c *SimulatedCanvas) hit(h string) {
	c.mu.Lock()
	c.hits = append(c.hits, h)
	c.mu.Unlock()
}

func (c *SimulatedCanvas) bodyOf(id int) []byte {
	if b, ok := c.contents[id]; ok {
		return b
	}
	return []byte(fmt.Sprintf("bytes of %d", id))
}

func (c *SimulatedCanvas) file(id int, name string) map[string]any {
	return map[string]any{"id": id, "display_name": name, "filename": name, "size": len(c.bodyOf(id)),
		"content-type": "application/pdf", "url": fmt.Sprintf("%s/dl/%d", c.Base, id)}
}

func respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func errs(message string) map[string]any {
	return map[string]any{"errors": []any{map[string]any{"message": message}}}
}

func (c *SimulatedCanvas) serve(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/dl/") {
		id := strings.TrimPrefix(r.URL.Path, "/dl/")
		c.hit("download " + id)
		n, _ := strconv.Atoi(id)
		w.Write(c.bodyOf(n))
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/v1/") {
		http.NotFound(w, r)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	c.hit(path)
	if r.Header.Get("Authorization") != simulatedCanvasToken {
		respond(w, 401, errs("Invalid access token"))
		return
	}
	q := r.URL.Query()
	base := c.Base
	data := map[string]any{
		"users/self/todo":            []any{map[string]any{"type": "submitting", "assignment": map[string]any{"name": "Lab 1"}}},
		"users/self/upcoming_events": []any{},
		"users/self/groups":          []any{},
		"courses/1/assignments": []any{map[string]any{"id": 11, "name": "Lab 1", "description": `<a href="/courses/1/files/99">x</a>`,
			"submission": map[string]any{"workflow_state": "unsubmitted"}}},
		"courses/1/modules": []any{map[string]any{"id": 21, "name": "Week 1"}},
		"courses/1/modules/21/items": []any{map[string]any{"id": 31, "type": "File", "content_id": 98, "title": "Slides"},
			map[string]any{"id": 32, "type": "Page", "page_url": "course-syllabus", "title": "0. Syllabus"}},
		"courses/1/files": []any{c.file(97, "syllabus.pdf"), c.file(96, "lab-report.pdf"),
			c.file(94, "active-voice-exercise.docx"), c.file(93, "transcript.txt")},
		"courses/1/folders":                      []any{map[string]any{"id": 41, "name": "course files"}},
		"folders/41/files":                       []any{c.file(97, "syllabus.pdf")},
		"courses/1/discussion_topics":            []any{map[string]any{"id": 51, "title": "Intro", "message": "hi", "discussion_subentry_count": 0}},
		"courses/1/discussion_topics/51/entries": []any{},
		"courses/1/pages":                        []any{map[string]any{"url": "welcome", "title": "Welcome"}},
		// Linked from a module but missing from the Pages list (as in courses that hide Pages)
		"courses/1/pages/course-syllabus": map[string]any{"url": "course-syllabus", "body": "Grading: 40% labs"},
		"courses/1/pages/welcome": map[string]any{"url": "welcome", "body": `Welcome! <a href="/courses/1/files/97/download?wrap=1">Syllabus</a> ` +
			`<a href="` + base + `/courses/1/files/96?verifier=abc">Lab</a> <img src="/courses/1/files/98/preview"> ` +
			`<a href="https://example.org/files/5">elsewhere</a> ` +
			`<a href="` + base + `/courses/1/files/99/?location=course_syllabus_1&amp;wrap=1">Handout</a> ` +
			`<img src="https://instructure-uploads.s3.amazonaws.com/account_1/attachments/95/photo.png` +
			`?X-Amz-Signature=abc&amp;X-Amz-Expires=600">`},
		"courses/1/quizzes":     []any{},
		"courses/1/enrollments": []any{map[string]any{"grades": map[string]any{"current_score": 93.5}}},
		"files/98":              c.file(98, "week1-slides.pdf"),
		"files/99":              c.file(99, "linked-handout.pdf"),
		"files/95":              c.file(95, "photo.png"), // only reachable through a Canvas storage link
		"files/94":              c.file(94, "active-voice-exercise.docx"),
		"calendar_events":       []any{map[string]any{"title": "Today", "start_at": "2026-10-21T10:00:00Z"}},
	}
	data["users/self"] = map[string]any{"id": 7, "name": "Student"}
	switch {
	case path == "courses":
		courses := []any{map[string]any{"id": 1, "name": "Intro AI"}, map[string]any{"id": 2, "name": "Chemistry"}}
		if q.Get("enrollment_state") == "completed" {
			courses = []any{}
		}
		// Paginate one course per page, like Canvas with a small per_page
		page := 1
		if p := q.Get("page"); p != "" {
			page, _ = strconv.Atoi(p)
		}
		var slice []any
		if page-1 < len(courses) {
			slice = courses[page-1 : page]
		} else {
			slice = []any{}
		}
		if page < len(courses) {
			w.Header().Set("Link", fmt.Sprintf(`<%s/api/v1/courses?page=%d&enrollment_state=%s>; rel="next"`, base, page+1, q.Get("enrollment_state")))
		}
		respond(w, 200, slice)
	case path == "courses/2/files": // Files tab hidden in this course
		respond(w, 401, map[string]any{"status": "unauthorized", "errors": []any{map[string]any{"message": "user not authorized"}}})
	case strings.HasPrefix(path, "courses/2/"):
		respond(w, 200, []any{})
	case path == "calendar_events" && q.Get("all_events") == "true":
		respond(w, 200, []any{map[string]any{"title": "Midterm", "start_at": "2026-11-05T09:00:00Z"},
			map[string]any{"title": "Final", "start_at": "2026-12-10T09:00:00Z"}})
	case path == "announcements":
		codes := q["context_codes[]"]
		switch {
		case len(codes) == 0:
			respond(w, 400, errs("context_codes is required"))
		case len(codes) == 1 && codes[0] == "course_1":
			respond(w, 200, []any{map[string]any{"title": "Welcome to AI", "posted_at": "2026-09-01T00:00:00Z", "message": "x"}})
		default:
			respond(w, 200, []any{map[string]any{"title": "Lab safety", "posted_at": "2026-10-01T00:00:00Z", "message": "y"}})
		}
	default:
		if body, ok := data[path]; ok {
			respond(w, 200, body)
		} else {
			respond(w, 404, errs("not found"))
		}
	}
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func run(t *testing.T, tool tools.Tool, args map[string]any) tools.ToolResult {
	t.Helper()
	out, err := tools.Call(context.Background(), tool, args)
	if err != nil {
		t.Fatalf("%s: %v", tool.Name(), err)
	}
	return out.(tools.ToolResult)
}

func output(t *testing.T, tool tools.Tool, args map[string]any) string {
	t.Helper()
	r := run(t, tool, args)
	if r.Error != "" {
		t.Fatalf("%s failed: %s", tool.Name(), r.Error)
	}
	return r.Output.(string)
}

func readLines(t *testing.T, path string) []map[string]any {
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		var m map[string]any
		json.Unmarshal(scanner.Bytes(), &m)
		out = append(out, m)
	}
	return out
}

func httpGet(t *testing.T, target string, headers map[string]string) (int, []byte, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body, resp.Header
}

func TestCaptureThenServeEverythingLocally(t *testing.T) {
	tmp := t.TempDir()
	canvas := NewSimulatedCanvas()
	defer canvas.Close()
	filesPort := freePort(t)
	public := fmt.Sprintf("http://127.0.0.1:%d", filesPort) // stands in for https://canvas.marksong.tech
	for k, v := range map[string]string{"CANVAS_SANDBOX": "yes", "CANVAS_SANDBOX_DIR": tmp, "CANVAS_URL": canvas.Base,
		"CANVAS_SANDBOX_PUBLIC_URL": public, "CANVAS_SANDBOX_FILES_PORT": strconv.Itoa(filesPort), "CANVAS_ACCESS_TOKEN": "test-token"} {
		t.Setenv(k, v)
	}
	sandbox.StopRunning()
	defer sandbox.StopRunning()
	ctx := context.Background()

	// --- Start(): no sandbox yet, so it captures everything first
	localURL, err := sandbox.Start(ctx, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("CANVAS_URL") != localURL || localURL == canvas.Base {
		t.Fatalf("CANVAS_URL %q, local %q", os.Getenv("CANVAS_URL"), localURL)
	}
	// --- after the capture nothing in the process holds the real Canvas URL or token
	simulated := os.Getenv("CANVAS_ACCESS_TOKEN")
	if !strings.HasPrefix(simulated, "sandbox-") {
		t.Fatalf("token %q", simulated)
	}
	for _, kv := range os.Environ() {
		_, v, _ := strings.Cut(kv, "=")
		if strings.Contains(v, canvas.Base) || v == "test-token" {
			t.Fatalf("real credentials still in the environment: %s", kv)
		}
	}
	for _, c := range []struct {
		auth string
		want int
	}{{simulatedCanvasToken, 401}, {"", 401}, {"Bearer " + simulated, 200}} {
		headers := map[string]string{}
		if c.auth != "" {
			headers["Authorization"] = c.auth
		}
		if status, _, _ := httpGet(t, localURL+"/api/v1/users/self", headers); status != c.want {
			t.Fatalf("%q -> %d", c.auth, status)
		}
	}
	if info, err := os.Stat(filepath.Join(tmp, "link_secret")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("link key is private to the sandbox: %v", err)
	}
	store, err := sandbox.NewStore(tmp).Load()
	if err != nil {
		t.Fatal(err)
	}
	if store.Manifest["complete"] != true || fmt.Sprint(store.Manifest["user_id"]) != "7" {
		t.Fatalf("manifest %v", store.Manifest)
	}
	courses := store.Body(sandbox.RequestKey("GET", "courses", []sandbox.Param{{Key: "enrollment_state", Value: "active"}})).([]any)
	if len(courses) != 2 || courses[0].(map[string]any)["name"] != "Intro AI" || courses[1].(map[string]any)["name"] != "Chemistry" {
		t.Fatalf("pages must be concatenated: %v", courses)
	}
	if store.Get(sandbox.RequestKey("GET", "courses/2/files", nil)).Status != 401 {
		t.Fatal("errors are replayed")
	}
	// Files: listed, in a folder, linked from a module item and from assignment HTML, all downloaded
	var ids []string
	for id := range store.Files {
		ids = append(ids, id)
	}
	if len(ids) != 7 {
		t.Fatalf("files %v", ids)
	}
	for _, id := range []string{"93", "94", "95", "96", "97", "98", "99"} {
		if store.Files[id] == nil {
			t.Fatalf("file %s missing", id)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(tmp, store.Files["99"].Path)); string(data) != "bytes of 99" {
		t.Fatalf("file 99: %q", data)
	}
	hitsAfterCapture := len(canvas.Hits())

	// --- the real tools, now answered by the sandbox
	out := output(t, canvastools.NewListCourses(), nil)
	if !strings.Contains(out, "Intro AI") || !strings.Contains(out, "Chemistry") {
		t.Fatal(out)
	}
	out = output(t, canvastools.NewGetFiles(), map[string]any{"course_id": "1", "search_term": "report"})
	if !strings.Contains(out, "lab-report.pdf") || strings.Contains(out, "syllabus.pdf") {
		t.Fatalf("search_term is applied locally: %s", out)
	}
	if r := run(t, canvastools.NewGetFiles(), map[string]any{"course_id": "2"}); !strings.Contains(r.Error, "401") {
		t.Fatalf("the hidden Files tab answers like Canvas did: %+v", r)
	}
	if r := run(t, canvastools.NewSearchFiles(), map[string]any{"course_id": "2", "search_term": "syllabus"}); !strings.Contains(r.Error, "401") {
		t.Fatalf("searching a hidden Files tab is refused the same way: %+v", r)
	}
	out = output(t, canvastools.NewGetCalendarEvents(), map[string]any{"start_date": "2026-11-01", "end_date": "2026-11-30"})
	if !strings.Contains(out, "Midterm") || strings.Contains(out, "Final") {
		t.Fatalf("date ranges are applied locally: %s", out)
	}
	out = output(t, canvastools.NewGetAnnouncements(), map[string]any{"context_codes": "course_1,course_2"})
	if !strings.Contains(out, "Welcome to AI") || !strings.Contains(out, "Lab safety") {
		t.Fatal(out)
	}
	// --- file links point at the platform (signed, expiring), never at Canvas
	out = output(t, canvastools.NewGetFileInfo(), map[string]any{"file_id": "99"})
	link := regexp.MustCompile(`Download link: (\S+)`).FindStringSubmatch(out)[1]
	if !strings.HasPrefix(link, public+"/files/99/linked-handout.pdf?expires=") || !strings.Contains(link, "&sig=") {
		t.Fatalf("link %s", link)
	}
	status, body, headers := httpGet(t, link, nil)
	if status != 200 || string(body) != "bytes of 99" || !strings.Contains(headers.Get("Content-Disposition"), "linked-handout.pdf") {
		t.Fatalf("download: %d %q %v", status, body, headers)
	}
	if status, _, _ := httpGet(t, link[:len(link)-4]+"0000", nil); status != 403 {
		t.Fatal("a tampered signature is refused")
	}
	if status, _, _ := httpGet(t, regexp.MustCompile(`expires=\d+`).ReplaceAllString(link, "expires=1000"), nil); status != 403 {
		t.Fatal("an expired link is refused")
	}
	if status, _, _ := httpGet(t, public+"/api/v1/users/self", nil); status != 404 {
		t.Fatal("the public server serves files only")
	}
	if status, _, _ := httpGet(t, localURL+"/files/99/download", nil); status != 403 {
		t.Fatal("unsigned downloads are refused everywhere")
	}
	page := output(t, canvastools.NewGetPageContent(), map[string]any{"course_id": "1", "page_url": "welcome"})
	if strings.Contains(page, canvas.Base) || strings.Contains(page, "/courses/1/files/") {
		t.Fatalf("no Canvas file link is left: %s", page)
	}
	if strings.Count(page, public+"/files/") != 5 || !strings.Contains(page, "https://example.org/files/5") {
		t.Fatalf("page links: %s", page)
	}
	if strings.Contains(page, "amazonaws") {
		t.Fatal("Canvas storage links are rewritten too")
	}
	out = output(t, canvastools.NewGetModuleItems(), map[string]any{"course_id": "1", "module_id": "21"})
	if !strings.Contains(out, "File ID: 98") || !strings.Contains(out, "Page URL: course-syllabus") {
		t.Fatal(out)
	}
	if out = output(t, canvastools.NewGetPageContent(), map[string]any{"course_id": "1", "page_url": "course-syllabus"}); !strings.Contains(out, "40% labs") {
		t.Fatal("pages linked only from modules are captured too")
	}
	if out = output(t, canvastools.NewGetGrades(), map[string]any{"course_id": "1"}); !strings.Contains(out, "93.5") {
		t.Fatal(out)
	}
	if r := run(t, canvastools.NewGetQuizzes(), map[string]any{"course_id": "404404"}); r.Error == "" {
		t.Fatal("requests that were never captured fail instead of reaching Canvas")
	}

	// --- the model can read files' text, from the sandbox's own copies
	canvastools.TextCache.Purge()
	out = output(t, canvastools.NewReadFile(), map[string]any{"file_id": "94"})
	if !strings.Contains(out, "Master the Active Voice Exercise") || !strings.Contains(out, "1. The essay was written by Maria.") {
		t.Fatal(out)
	}
	if first := output(t, canvastools.NewReadFile(), map[string]any{"file_id": "93"}); !strings.Contains(first, "start=12000") {
		t.Fatal("long files come in parts")
	}
	rest := output(t, canvastools.NewReadFile(), map[string]any{"file_id": "93", "start": 12000})
	if !strings.Contains(rest, "showing from character 12000") || strings.Contains(rest, "More text follows") {
		t.Fatal(rest)
	}
	if out = output(t, canvastools.NewReadFile(), map[string]any{"file_id": "96"}); !strings.Contains(out, "couldn't be read") {
		t.Fatalf("a broken file says so instead of failing: %s", out)
	}
	if r := run(t, canvastools.NewReadFile(), map[string]any{"file_id": "404404"}); r.Error == "" {
		t.Fatal("a file that was never captured is an error, not a Canvas request")
	}

	// --- writes are recorded locally and show up in later reads
	out = output(t, canvastools.NewSubmitAssignment(), map[string]any{"course_id": "1", "assignment_id": "11",
		"submission_type": "online_text_entry", "body": "My answer"})
	if !strings.Contains(out, "SANDBOX") {
		t.Fatal(out)
	}
	assignments := canvastools.MakeRequest(ctx, "GET", "courses/1/assignments", nil, nil).([]any)
	if assignments[0].(map[string]any)["submission"].(map[string]any)["workflow_state"] != "submitted" {
		t.Fatalf("assignments %v", assignments)
	}
	if out = output(t, canvastools.NewPostDiscussion(), map[string]any{"course_id": "1", "topic_id": "51", "message": "Hello class"}); !strings.Contains(out, "SANDBOX") {
		t.Fatal(out)
	}
	writes := readLines(t, filepath.Join(tmp, "writes.jsonl"))
	if len(writes) != 2 || writes[0]["path"] != "courses/1/assignments/11/submissions" || writes[1]["path"] != "courses/1/discussion_topics/51/entries" {
		t.Fatalf("writes %v", writes)
	}
	if writes[0]["body"].(map[string]any)["submission"].(map[string]any)["body"] != "My answer" {
		t.Fatalf("write body %v", writes[0]["body"])
	}
	// Survives a restart: the recorded submission is saved in the sandbox
	reloaded, _ := sandbox.NewStore(tmp).Load()
	saved := reloaded.Body(sandbox.RequestKey("GET", "courses/1/assignments", nil)).([]any)
	if saved[0].(map[string]any)["submission"].(map[string]any)["sandbox"] != true {
		t.Fatal("the submission should be saved")
	}

	if len(canvas.Hits()) != hitsAfterCapture {
		t.Fatalf("nothing may reach Canvas after the capture: %v", canvas.Hits()[hitsAfterCapture:])
	}
	outcomes := map[string]bool{}
	for _, entry := range readLines(t, filepath.Join(tmp, "requests.jsonl")) {
		outcomes[fmt.Sprint(entry["outcome"])] = true
	}
	for _, want := range []string{"hit", "emulated", "miss", "write", "file", "unauthorized"} {
		if !outcomes[want] {
			t.Fatalf("outcome %q not logged: %v", want, outcomes)
		}
	}

	// --- a restart with only simulated credentials serves the captured sandbox (no real ones needed)
	sandbox.StopRunning()
	os.Setenv("CANVAS_URL", "http://sandbox.invalid")
	os.Setenv("CANVAS_ACCESS_TOKEN", "sandbox-placeholder")
	restarted, err := sandbox.Start(ctx, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if out = output(t, canvastools.NewListCourses(), nil); !strings.Contains(out, "Intro AI") {
		t.Fatal(out)
	}
	if os.Getenv("CANVAS_URL") != restarted || len(canvas.Hits()) != hitsAfterCapture {
		t.Fatal("the restart must not reach Canvas")
	}
}

func TestLiveModeBlocksWritesBeforeSending(t *testing.T) {
	canvas := NewSimulatedCanvas()
	defer canvas.Close()
	sandbox.StopRunning()
	t.Setenv("CANVAS_SANDBOX", "no")
	t.Setenv("CANVAS_URL", canvas.Base) // live mode, pointed at "Canvas"
	t.Setenv("CANVAS_ACCESS_TOKEN", "test-token")
	submit := run(t, canvastools.NewSubmitAssignment(), map[string]any{"course_id": "1", "assignment_id": "11",
		"submission_type": "online_text_entry", "body": "My answer"})
	post := run(t, canvastools.NewPostDiscussion(), map[string]any{"course_id": "1", "topic_id": "51", "message": "Hello class"})
	reads := output(t, canvastools.NewListCourses(), nil)
	canvastools.TextCache.Purge() // the sandbox scenario may have cached this file's text
	worksheet := output(t, canvastools.NewReadFile(), map[string]any{"file_id": "94"})
	if submit.Output != nil || !strings.HasPrefix(submit.Error, "Blocked") || post.Output != nil || !strings.HasPrefix(post.Error, "Blocked") {
		t.Fatalf("writes: %+v %+v", submit, post)
	}
	if !strings.Contains(reads, "Intro AI") {
		t.Fatal("reads still go to Canvas")
	}
	if !strings.Contains(worksheet, "2. Mistakes were made.") {
		t.Fatalf("live mode reads files through Canvas's download link: %s", worksheet)
	}
	for _, hit := range canvas.Hits() {
		if strings.HasPrefix(hit, "courses/1/assignments/11/submissions") || strings.Contains(hit, "entries") {
			t.Fatalf("no write may reach Canvas: %s", hit)
		}
	}
}

func TestWriteToolsAlwaysOffered(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range cfg.Tools {
		names[tool.Name()] = true
	}
	if !names["canvas_submit_assignment"] || !names["canvas_post_discussion"] {
		t.Fatal("write tools must be offered")
	}
}

func TestSandboxOffByDefault(t *testing.T) {
	os.Unsetenv("CANVAS_SANDBOX")
	if sandbox.Enabled() {
		t.Fatal("sandbox mode must be off by default")
	}
	if url, err := sandbox.Start(context.Background(), nil); url != "" || err != nil {
		t.Fatalf("Start: %q %v", url, err)
	}
}

func TestNullFieldsUseFallbackText(t *testing.T) {
	canvas := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/courses":
			respond(w, 200, []any{map[string]any{"id": 1, "name": "Intro AI", "course_code": nil}})
		case "/api/v1/courses/1/assignments":
			respond(w, 200, []any{map[string]any{"id": 11, "name": "Lab 1", "due_at": nil, "points_possible": nil, "submission": nil}})
		default:
			respond(w, 404, errs("not found"))
		}
	}))
	defer canvas.Close()
	sandbox.StopRunning()
	t.Setenv("CANVAS_SANDBOX", "no")
	t.Setenv("CANVAS_URL", canvas.URL)
	t.Setenv("CANVAS_ACCESS_TOKEN", "test-token")
	courses := output(t, canvastools.NewListCourses(), nil)
	assignments := output(t, canvastools.NewGetAssignments(), map[string]any{"course_id": "1"})
	if !strings.Contains(courses, "- [1] Intro AI (unknown)") {
		t.Fatalf("courses: %s", courses)
	}
	if !strings.Contains(assignments, "(Due: no due date, Points: 0, Status: unsubmitted)") {
		t.Fatalf("assignments: %s", assignments)
	}
	for _, out := range []string{courses, assignments} {
		if strings.Contains(out, "None") || strings.Contains(out, "<nil>") {
			t.Fatalf("a null field leaked into the output: %s", out)
		}
	}
}
