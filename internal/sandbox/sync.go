package sandbox

/*
Capture everything the Canvas tools could ask for into a Store, using the real Canvas API and token, then
download every file.

Crawl plan (all list endpoints are followed to the last page):
  - the user, to-dos, upcoming events, groups, calendar events (as the tool asks, and all events for date filters)
  - courses in every enrollment state, with every include the course tool can request
  - per course: assignments (with submissions), modules and their items, files, folders and their files,
    discussion topics and their entries, pages and each page's content, quizzes, the student's enrollment
    (grades), and announcements over the course's whole history (Canvas defaults to the last 14 days)
  - files referenced anywhere (module items, links in syllabus/assignment/page/discussion/announcement HTML),
    so courses that hide their Files tab still get their linked files

Failed endpoints (e.g. a hidden Files tab answers 401) are stored with their status, so the sandbox replays the
same error the tool would have seen.
*/

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"canvas_go/internal/jsonval"
	"canvas_go/internal/textutil"
)

var (
	courseStates   = []string{"", "active", "completed", "invited_or_pending"}
	courseIncludes = []string{"total_students", "teachers", "syllabus_body"}
	// File ids in links: /files/<id> on Canvas, and Canvas's file storage (instructure-uploads...amazonaws.com/
	// .../attachments/<id>/...), where the attachment id is the file id
	fileLinkRE   = regexp.MustCompile(`/files/(\d+)|instructure-uploads[\w.-]*\.amazonaws\.com/[^\s"'<>]*?/attachments/(\d+)/`)
	unsafeNameRE = regexp.MustCompile(`[\\/:*?"<>|\x00-\x1f]`)
)

const (
	Concurrency = 4
	PerPage     = 100
)

func okList(status int, body any) []any {
	if list, ok := body.([]any); ok && status == 200 {
		return list
	}
	return nil
}

func nextLink(linkHeader string) string {
	for _, part := range strings.Split(linkHeader, ",") {
		if strings.Contains(part, `rel="next"`) {
			start, end := strings.Index(part, "<"), strings.Index(part, ">")
			if start >= 0 && end > start {
				return part[start+1 : end]
			}
		}
	}
	return ""
}

// SafeName makes a file name safe for any file system.
func SafeName(name string) string {
	if name == "" {
		name = "unnamed"
	}
	name = strings.Trim(unsafeNameRE.ReplaceAllString(name, "_"), " .")
	name = textutil.Substr(name, 0, 180)
	if name == "" {
		return "unnamed"
	}
	return name
}

// Crawler captures a Canvas account into a Store.
type Crawler struct {
	store        *Store
	api          string
	token        string
	log          func(string)
	semaphore    chan struct{}
	client       *http.Client
	mu           sync.Mutex
	errors       map[string]any
	fileMeta     map[string]map[string]any
	linkedFiles  map[string]bool
	requestCount int
}

func NewCrawler(store *Store, canvasURL, token string, log func(string)) *Crawler {
	return &Crawler{
		store: store, api: strings.TrimRight(canvasURL, "/") + "/api/v1", token: token, log: log,
		semaphore: make(chan struct{}, Concurrency), client: &http.Client{},
		errors: map[string]any{}, fileMeta: map[string]map[string]any{}, linkedFiles: map[string]bool{},
	}
}

// Fetch GETs every page of path, stores the result under key (default: the request's key) and returns it.
func (c *Crawler) Fetch(ctx context.Context, path string, params []Param, key string) (int, any) {
	if key == "" {
		key = RequestKey("GET", path, params)
	}
	query := url.Values{}
	var parts []string
	for _, p := range append(append([]Param(nil), params...), Param{"per_page", fmt.Sprint(PerPage)}) {
		parts = append(parts, url.QueryEscape(p.Key)+"="+url.QueryEscape(p.Value))
	}
	_ = query
	next := c.api + "/" + path + "?" + strings.Join(parts, "&")
	var items []any
	status, body := 0, any(nil)
	for next != "" {
		var link string
		status, body, link = c.get(ctx, next)
		if status != 200 {
			break
		}
		if list, ok := body.([]any); ok {
			items = append(items, list...)
			next = nextLink(link) // the next-page link carries the query
		} else {
			next = ""
		}
	}
	if _, isList := body.([]any); status == 200 && isList {
		if items == nil {
			items = []any{}
		}
		body = items
	}
	if status != 200 {
		c.mu.Lock()
		c.errors[key] = status
		c.mu.Unlock()
	}
	c.store.Put(key, status, body)
	return status, body
}

func (c *Crawler) get(ctx context.Context, target string) (int, any, string) {
	var status int
	var text, link string
	for attempt := 0; attempt < 5; attempt++ {
		c.semaphore <- struct{}{}
		c.mu.Lock()
		c.requestCount++
		c.mu.Unlock()
		status, text, link = c.request(ctx, target)
		<-c.semaphore
		if (status == 403 && strings.Contains(text, "Rate Limit Exceeded")) || status == 429 || status == 599 {
			time.Sleep(time.Duration(math.Pow(2, float64(attempt))) * time.Second) // back off and retry
			continue
		}
		break
	}
	var body any
	if text != "" {
		parsed, err := jsonval.Decode([]byte(text))
		if err != nil {
			body = errorBody(textutil.Substr(text, 0, 300))
		} else {
			body = parsed
		}
	}
	return status, body, link
}

func (c *Crawler) request(ctx context.Context, target string) (int, string, string) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err == nil {
		req.Header.Set("Authorization", "Bearer "+c.token)
		var resp *http.Response
		if resp, err = c.client.Do(req); err == nil {
			defer resp.Body.Close()
			data, readErr := io.ReadAll(resp.Body)
			if readErr == nil {
				return resp.StatusCode, textutil.DecodeUTF8(data), resp.Header.Get("Link")
			}
			err = readErr
		}
	}
	return 599, jsonval.JSON(errorBody(fmt.Sprintf("Request error: %v", err))), ""
}

func (c *Crawler) scanHTML(html any) {
	text, ok := html.(string)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range fileLinkRE.FindAllStringSubmatch(text, -1) {
		if m[1] != "" {
			c.linkedFiles[m[1]] = true
		} else {
			c.linkedFiles[m[2]] = true
		}
	}
}

func (c *Crawler) addFiles(status int, body any) {
	for _, item := range okList(status, body) {
		meta, _ := item.(map[string]any)
		if meta == nil {
			continue
		}
		fileID := jsonval.Text(meta["id"])
		c.mu.Lock()
		c.fileMeta[fileID] = meta
		c.mu.Unlock()
		// A listed file object is what GET files/<id> returns, so it answers that request too
		c.store.Put(RequestKey("GET", "files/"+fileID, nil), 200, meta)
	}
}

func parallel(fns ...func()) {
	var wg sync.WaitGroup
	for _, fn := range fns {
		wg.Add(1)
		go func(fn func()) {
			defer wg.Done()
			fn()
		}(fn)
	}
	wg.Wait()
}

// Crawl captures everything and returns the manifest.
func (c *Crawler) Crawl(ctx context.Context) (map[string]any, error) {
	started := time.Now()
	status, me := c.Fetch(ctx, "users/self", nil, "")
	if status != 200 {
		return nil, fmt.Errorf("Canvas rejected the access token (HTTP %d); nothing was captured.", status)
	}
	userID := me.(map[string]any)["id"]
	parallel(
		func() { c.Fetch(ctx, "users/self/todo", nil, "") },
		func() { c.Fetch(ctx, "users/self/upcoming_events", nil, "") },
		func() { c.Fetch(ctx, "users/self/groups", nil, "") },
		func() { c.Fetch(ctx, "calendar_events", nil, "") },
		// Every event, so the sandbox can answer any start_date / end_date range itself
		func() { c.Fetch(ctx, "calendar_events", []Param{{"all_events", "true"}}, "") },
		func() { c.Fetch(ctx, "announcements", nil, "") },
	)

	courses := map[string]map[string]any{}
	var courseOrder []string
	for _, state := range courseStates {
		var params []Param
		for _, inc := range courseIncludes {
			params = append(params, Param{"include[]", inc})
		}
		if state != "" {
			params = append(params, Param{"enrollment_state", state})
		}
		for _, item := range okList(c.Fetch(ctx, "courses", params, "")) {
			course, _ := item.(map[string]any)
			if course == nil {
				continue
			}
			id := jsonval.Text(course["id"])
			if _, seen := courses[id]; !seen {
				courseOrder = append(courseOrder, id)
			}
			courses[id] = course
		}
	}
	c.log(fmt.Sprintf("Sandbox sync: %d courses; capturing each one...", len(courses)))

	done := 0
	var doneMu sync.Mutex
	var fns []func()
	for _, id := range courseOrder {
		course := courses[id]
		fns = append(fns, func() {
			c.crawlCourse(ctx, course, userID)
			doneMu.Lock()
			done++
			if done%10 == 0 || done == len(courses) {
				c.mu.Lock()
				count := c.requestCount
				c.mu.Unlock()
				c.log(fmt.Sprintf("Sandbox sync: %d/%d courses captured (%d requests)", done, len(courses), count))
			}
			doneMu.Unlock()
		})
	}
	parallel(fns...)

	// Files only reachable through links (hidden Files tab, module items, embedded links)
	var missing []string
	for id := range c.linkedFiles {
		if _, known := c.fileMeta[id]; !known {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	c.log(fmt.Sprintf("Sandbox sync: fetching metadata for %d linked files...", len(missing)))
	fns = nil
	for _, id := range missing {
		fns = append(fns, func() {
			status, meta := c.Fetch(ctx, "files/"+id, nil, "")
			if m, ok := meta.(map[string]any); ok && status == 200 {
				c.mu.Lock()
				c.fileMeta[jsonval.Text(m["id"])] = m
				c.mu.Unlock()
			}
		})
	}
	parallel(fns...)

	downloaded, unavailable, failed := c.downloadFiles(ctx)
	c.store.mu.Lock()
	responses := len(c.store.Responses)
	c.store.mu.Unlock()
	errorsCopy := map[string]any{}
	for k, v := range c.errors {
		errorsCopy[k] = v
	}
	manifest := map[string]any{
		"complete":   true,
		"created":    time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00"),
		"source":     strings.Split(c.api, "/api/v1")[0],
		"user_id":    userID,
		"duration_s": math.Round(time.Since(started).Seconds()*10) / 10,
		"requests":   c.requestCount,
		"counts": map[string]any{
			"responses":         responses,
			"courses":           len(courses),
			"files_known":       len(c.fileMeta),
			"files_downloaded":  downloaded,
			"files_unavailable": unavailable,
			"files_failed":      len(failed),
		},
		"errors":      errorsCopy,
		"file_errors": failed,
	}
	c.store.Manifest = manifest
	return manifest, nil
}

func (c *Crawler) crawlCourse(ctx context.Context, course map[string]any, userID any) {
	cid := jsonval.Text(course["id"])
	base := "courses/" + cid
	c.scanHTML(course["syllabus_body"])

	for _, item := range okList(c.Fetch(ctx, base+"/assignments", []Param{{"include[]", "submission"}}, "")) {
		if a, ok := item.(map[string]any); ok {
			c.scanHTML(a["description"])
		}
	}

	linkedPages := map[string]bool{}
	for _, m := range okList(c.Fetch(ctx, base+"/modules", nil, "")) {
		module, _ := m.(map[string]any)
		if module == nil {
			continue
		}
		for _, it := range okList(c.Fetch(ctx, fmt.Sprintf("%s/modules/%s/items", base, jsonval.Text(module["id"])), nil, "")) {
			item, _ := it.(map[string]any)
			if item == nil {
				continue
			}
			if item["type"] == "File" && jsonval.Truthy(item["content_id"]) {
				c.mu.Lock()
				c.linkedFiles[jsonval.Text(item["content_id"])] = true
				c.mu.Unlock()
			} else if item["type"] == "Page" && jsonval.Truthy(item["page_url"]) {
				linkedPages[jsonval.Text(item["page_url"])] = true
			}
		}
	}

	c.addFiles(c.Fetch(ctx, base+"/files", nil, ""))
	for _, f := range okList(c.Fetch(ctx, base+"/folders", nil, "")) {
		if folder, ok := f.(map[string]any); ok {
			c.addFiles(c.Fetch(ctx, "folders/"+jsonval.Text(folder["id"])+"/files", nil, ""))
		}
	}

	for _, t := range okList(c.Fetch(ctx, base+"/discussion_topics", nil, "")) {
		topic, _ := t.(map[string]any)
		if topic == nil {
			continue
		}
		c.scanHTML(topic["message"])
		for _, e := range okList(c.Fetch(ctx, fmt.Sprintf("%s/discussion_topics/%s/entries", base, jsonval.Text(topic["id"])), nil, "")) {
			if entry, ok := e.(map[string]any); ok {
				c.scanHTML(entry["message"])
			}
		}
	}

	pages := map[string]bool{}
	for _, p := range okList(c.Fetch(ctx, base+"/pages", nil, "")) {
		if page, ok := p.(map[string]any); ok {
			pages[jsonval.Text(page["url"])] = true
		}
	}
	// Pages linked from modules count too: many courses hide the Pages list but link pages in modules
	for p := range linkedPages {
		pages[p] = true
	}
	var pageURLs []string
	for p := range pages {
		pageURLs = append(pageURLs, p)
	}
	sort.Strings(pageURLs)
	for _, pageURL := range pageURLs {
		status, content := c.Fetch(ctx, base+"/pages/"+pageURL, nil, "")
		if m, ok := content.(map[string]any); ok && status == 200 {
			c.scanHTML(m["body"])
		}
	}

	c.Fetch(ctx, base+"/quizzes", nil, "")
	c.Fetch(ctx, base+"/enrollments", []Param{{"user_id", jsonval.Text(userID)}}, "")

	// The announcements tool asks per course; store the course's whole history under that request
	farFuture := time.Now().AddDate(0, 0, 730).Format("2006-01-02")
	status, announcements := c.Fetch(ctx, "announcements",
		[]Param{{"context_codes[]", "course_" + cid}, {"start_date", "2000-01-01"}, {"end_date", farFuture}},
		RequestKey("GET", "announcements", []Param{{"context_codes[]", "course_" + cid}}))
	for _, a := range okList(status, announcements) {
		if announcement, ok := a.(map[string]any); ok {
			c.scanHTML(announcement["message"])
		}
	}
}

func (c *Crawler) downloadFiles(ctx context.Context) (int, int, map[string]any) {
	downloaded, unavailable := 0, 0
	failed := map[string]any{}
	filesRoot := filepath.Join(c.store.Root, "files")
	var mu sync.Mutex

	download := func(fileID string, meta map[string]any) {
		fileURL, _ := meta["url"].(string)
		if fileURL == "" || jsonval.Truthy(meta["locked_for_user"]) || jsonval.Truthy(meta["hidden_for_user"]) {
			mu.Lock()
			unavailable++
			mu.Unlock()
			return
		}
		name := ""
		if jsonval.Truthy(meta["display_name"]) {
			name = jsonval.Text(meta["display_name"])
		} else if jsonval.Truthy(meta["filename"]) {
			name = jsonval.Text(meta["filename"])
		}
		path := filepath.Join(filesRoot, fileID, SafeName(name))
		size, hasSize := jsonval.Float(meta["size"])
		info, statErr := os.Stat(path)
		if !(statErr == nil && info.Mode().IsRegular() && hasSize && info.Size() == int64(size)) {
			if problem := c.fetchFile(ctx, fileURL, path); problem != "" {
				mu.Lock()
				failed[fileID] = problem
				mu.Unlock()
				return
			}
		}
		info, err := os.Stat(path)
		if err != nil {
			mu.Lock()
			failed[fileID] = textutil.Substr(err.Error(), 0, 200)
			mu.Unlock()
			return
		}
		rel, _ := filepath.Rel(c.store.Root, path)
		mu.Lock()
		downloaded++
		mu.Unlock()
		c.store.mu.Lock()
		c.store.Files[fileID] = &FileEntry{Path: rel, Size: info.Size(), ContentType: meta["content-type"], DisplayName: meta["display_name"]}
		c.store.mu.Unlock()
	}

	c.log(fmt.Sprintf("Sandbox sync: downloading %d files (existing complete files are kept)...", len(c.fileMeta)))
	var fns []func()
	for id, meta := range c.fileMeta {
		fns = append(fns, func() { download(id, meta) })
	}
	parallel(fns...)
	return downloaded, unavailable, failed
}

// fetchFile downloads url to path (via a .part file); returns a problem description or "".
func (c *Crawler) fetchFile(ctx context.Context, fileURL, path string) string {
	c.semaphore <- struct{}{}
	defer func() { <-c.semaphore }()
	ctx, cancel := context.WithTimeout(ctx, 600*time.Second)
	defer cancel()
	// No auth header: the URL is pre-signed and redirects to file storage on another host
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return textutil.Substr(err.Error(), 0, 200)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return textutil.Substr(err.Error(), 0, 200)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return textutil.Substr(err.Error(), 0, 200)
	}
	tmp := path + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return textutil.Substr(err.Error(), 0, 200)
	}
	_, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		os.Remove(tmp)
		if copyErr == nil {
			copyErr = closeErr
		}
		return textutil.Substr(copyErr.Error(), 0, 200)
	}
	if err := os.Rename(tmp, path); err != nil {
		return textutil.Substr(err.Error(), 0, 200)
	}
	return ""
}

// Sync captures Canvas into store (replacing earlier responses; downloaded files are reused) and saves it.
func Sync(ctx context.Context, store *Store, canvasURL, token string, log func(string)) (map[string]any, error) {
	if err := os.MkdirAll(store.Root, 0o755); err != nil {
		return nil, err
	}
	store.mu.Lock()
	store.Responses, store.Files, store.Manifest = map[string]*Response{}, map[string]*FileEntry{}, map[string]any{"complete": false}
	store.mu.Unlock()
	if err := store.Save(); err != nil { // marks the sandbox incomplete until this sync finishes
		return nil, err
	}
	manifest, err := NewCrawler(store, canvasURL, token, log).Crawl(ctx)
	if err != nil {
		return nil, err
	}
	if err := store.Save(); err != nil {
		return nil, err
	}
	counts := manifest["counts"].(map[string]any)
	log(fmt.Sprintf("Sandbox sync done in %vs: %v responses, %v courses, %v files downloaded (%v locked, %v failed), "+
		"%d endpoints answered with an error.", manifest["duration_s"], counts["responses"], counts["courses"],
		counts["files_downloaded"], counts["files_unavailable"], counts["files_failed"], len(manifest["errors"].(map[string]any))))
	return manifest, nil
}
