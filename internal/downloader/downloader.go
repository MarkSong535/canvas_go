// Package downloader is the Canvas full file index downloader: it downloads every file for each course,
// organizes them by course and module, and optionally uploads supported files to an OpenAI Vector Store.
package downloader

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openai/openai-go/v3"

	"canvas_go/internal/jsonval"
	"canvas_go/internal/models"
	"canvas_go/internal/textutil"
)

// DownloadRoot is the root directory for downloaded artifacts.
var DownloadRoot = "file_index"

// Out receives the downloader's console output.
var Out io.Writer = os.Stdout

// In supplies answers to interactive prompts.
var In io.Reader = os.Stdin

// ErrorEntry is one failed file.
type ErrorEntry struct {
	Course string `json:"course"`
	Module string `json:"module,omitempty"`
	File   string `json:"file"`
	Error  string `json:"error"`
}

// Stats are the aggregated run statistics.
type Stats struct {
	Courses                    int          `json:"courses"`
	Modules                    int          `json:"modules"`
	FilesTotal                 int          `json:"files_total"`
	FilesDownloaded            int          `json:"files_downloaded"`
	FilesSkipped               int          `json:"files_skipped"`
	FilesFailed                int          `json:"files_failed"`
	TotalSize                  int64        `json:"total_size"`
	VectorStoresCreated        int          `json:"vector_stores_created"`
	FilesUploadedToVectorStore int          `json:"files_uploaded_to_vector_store"`
	FilesUploadFailed          int          `json:"files_upload_failed"`
	Errors                     []ErrorEntry `json:"errors"`
}

var (
	mu    sync.Mutex
	stats = newStats()

	// Automation controls (used when driven programmatically, e.g. by the WebSocket server)
	autoMode            bool
	autoSelectedCourses map[int64]bool // nil = all
	autoConfirm         = true
)

func newStats() *Stats { return &Stats{Errors: []ErrorEntry{}} }

// CurrentStats returns a copy of the statistics.
func CurrentStats() Stats {
	mu.Lock()
	defer mu.Unlock()
	out := *stats
	out.Errors = append([]ErrorEntry{}, stats.Errors...)
	return out
}

// ConfigureAutomation enables automation mode so the downloader can run without prompts.
func ConfigureAutomation(courseIDs []int64, confirm bool) {
	mu.Lock()
	defer mu.Unlock()
	autoMode = true
	autoSelectedCourses = nil
	if len(courseIDs) > 0 {
		autoSelectedCourses = map[int64]bool{}
		for _, id := range courseIDs {
			autoSelectedCourses[id] = true
		}
	}
	autoConfirm = confirm
}

// ClearAutomation disables automation mode and restores defaults.
func ClearAutomation() {
	mu.Lock()
	defer mu.Unlock()
	autoMode, autoSelectedCourses, autoConfirm = false, nil, true
}

// ResetStats resets run statistics between automated invocations.
func ResetStats() {
	mu.Lock()
	defer mu.Unlock()
	stats = newStats()
}

// Vector Store configuration
var supportedExtensions = map[string]bool{".pdf": true, ".txt": true, ".md": true, ".doc": true, ".docx": true, ".ppt": true,
	".pptx": true, ".xls": true, ".xlsx": true, ".json": true, ".csv": true}

const MaxFileSize = 512 * 1024 * 1024 // 512 MB (OpenAI limit)

// SanitizeFilename removes characters that are illegal in file or folder names.
func SanitizeFilename(name string) string {
	for _, c := range `<>:"/\|?*` {
		name = strings.ReplaceAll(name, string(c), "_")
	}
	name = strings.Trim(name, ". ")
	if textutil.Len(name) > 200 {
		name = textutil.Substr(name, 0, 200)
	}
	if name == "" {
		return "unnamed"
	}
	return name
}

func printf(format string, args ...any) { fmt.Fprintf(Out, format, args...) }

// Param is one query parameter.
type Param struct{ Key, Value string }

func withQuery(target string, params []Param) string {
	if len(params) == 0 {
		return target
	}
	var parts []string
	for _, p := range params {
		parts = append(parts, url.QueryEscape(p.Key)+"="+url.QueryEscape(p.Value))
	}
	sep := "?"
	if strings.Contains(target, "?") {
		sep = "&"
	}
	return target + sep + strings.Join(parts, "&")
}

var client = &http.Client{}

// FetchAllPages retrieves all pages of a paginated Canvas API endpoint. With raiseOnError a failed request is
// returned as an error; otherwise it is printed and the pages so far are returned.
func FetchAllPages(ctx context.Context, target string, headers map[string]string, params []Param, raiseOnError bool) ([]map[string]any, error) {
	var all []map[string]any
	current := withQuery(target, params)
	for current != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err == nil {
			for k, v := range headers {
				req.Header.Set(k, v)
			}
		}
		var resp *http.Response
		if err == nil {
			resp, err = client.Do(req)
		}
		if err != nil {
			if raiseOnError {
				return nil, err
			}
			printf("❌ Request error: %v\n", err)
			break
		}
		data, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			if raiseOnError {
				message := fmt.Sprintf("Canvas API request failed (HTTP %d)", resp.StatusCode)
				var body map[string]any
				if json.Unmarshal(data, &body) == nil {
					if errs, ok := body["errors"].([]any); ok {
						var details []string
						for _, e := range errs {
							if m, ok := e.(map[string]any); ok {
								details = append(details, jsonval.Text(get(m, "message", "")))
							}
						}
						if detail := textutil.Substr(strings.Join(details, "; "), 0, 300); detail != "" {
							message += ": " + detail
						}
					}
				}
				return nil, fmt.Errorf("%s", message)
			}
			printf("⚠️  Request failed (%d): %s\n", resp.StatusCode, current)
			break
		}
		if readErr != nil {
			if raiseOnError {
				return nil, readErr
			}
			break
		}
		parsed, err := jsonval.Decode(data)
		if err != nil {
			if raiseOnError {
				return nil, err
			}
			printf("❌ Request error: %v\n", err)
			break
		}
		switch v := parsed.(type) {
		case []any:
			for _, item := range v {
				if m, ok := item.(map[string]any); ok {
					all = append(all, m)
				}
			}
		case map[string]any:
			all = append(all, v)
		}
		// Inspect pagination headers for the next page (subsequent requests carry their own query)
		current = ""
		for _, link := range strings.Split(resp.Header.Get("Link"), ",") {
			if strings.Contains(link, `rel="next"`) {
				current = link[strings.Index(link, "<")+1 : strings.Index(link, ">")]
				break
			}
		}
	}
	return all, nil
}

func get(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// downloadFile downloads one file; returns (ok, message).
func downloadFile(ctx context.Context, fileInfo map[string]any, path string) (bool, string) {
	fileURL, _ := fileInfo["url"].(string)
	size, _ := jsonval.Float(get(fileInfo, "size", 0))
	if fileURL == "" {
		return false, "Missing download URL"
	}
	// Skip downloads when the file already exists and matches the expected size
	if info, err := os.Stat(path); err == nil && info.Size() == int64(size) {
		mu.Lock()
		stats.FilesSkipped++
		mu.Unlock()
		return true, "Already exists"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return false, err.Error()
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false, fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err.Error()
	}
	f, err := os.Create(path)
	if err != nil {
		return false, err.Error()
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return false, err.Error()
	}
	if err := f.Close(); err != nil {
		return false, err.Error()
	}
	mu.Lock()
	stats.FilesDownloaded++
	stats.TotalSize += int64(size)
	mu.Unlock()
	return true, "Success"
}

func readLine(prompt string) string {
	printf("%s", prompt)
	line, _ := bufio.NewReader(In).ReadString('\n')
	return strings.TrimSpace(line)
}

// selectCourses asks which courses to download (or applies the automation selection).
func selectCourses(courses []map[string]any) []map[string]any {
	if len(courses) == 0 {
		return nil
	}
	mu.Lock()
	auto, selected := autoMode, autoSelectedCourses
	mu.Unlock()
	if auto {
		if selected == nil {
			return courses
		}
		var out []map[string]any
		for _, c := range courses {
			if id, ok := jsonval.Float(c["id"]); ok && selected[int64(id)] {
				out = append(out, c)
			}
		}
		return out
	}
	printf("Enter course indices using one of the formats below:\n")
	printf("  • Type `all` or press Enter to download every course\n")
	printf("  • Type a single index to select one course (for example: 3)\n")
	printf("  • Type multiple indices separated by commas (for example: 1,3,5)\n\n")
	for {
		choice := strings.ToLower(readLine("Select courses to download (default all): "))
		if choice == "" || choice == "all" {
			return courses
		}
		indices := map[int]bool{}
		valid := true
		for _, part := range strings.Split(choice, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			n, err := strconv.Atoi(part)
			if err != nil || n < 1 || n > len(courses) {
				valid = false
				break
			}
			indices[n-1] = true
		}
		if valid && len(indices) > 0 {
			var sorted []int
			for i := range indices {
				sorted = append(sorted, i)
			}
			sort.Ints(sorted)
			var out []map[string]any
			for _, i := range sorted {
				out = append(out, courses[i])
			}
			printf("✓ Selected %d course(s)\n\n", len(out))
			return out
		}
		printf("⚠️  Invalid input, enter course numbers or `all`\n")
	}
}

func fileInfo(ctx context.Context, canvasURL string, headers map[string]string, fileID string) map[string]any {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, canvasURL+"/api/v1/files/"+fileID, nil)
	if err != nil {
		return nil
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}
	parsed, err := jsonval.Decode(data)
	if err != nil {
		return nil
	}
	m, _ := parsed.(map[string]any)
	return m
}

func courseFiles(ctx context.Context, canvasURL string, headers map[string]string, courseID string) []map[string]any {
	files, err := FetchAllPages(ctx, canvasURL+"/api/v1/courses/"+courseID+"/files", headers, []Param{{"per_page", "100"}}, false)
	if err == nil {
		return files
	}
	// As a fallback, iterate through folders manually
	folders, err := FetchAllPages(ctx, canvasURL+"/api/v1/courses/"+courseID+"/folders", headers, []Param{{"per_page", "100"}}, false)
	if err != nil {
		return nil
	}
	var all []map[string]any
	for _, folder := range folders {
		folderFiles, _ := FetchAllPages(ctx, canvasURL+"/api/v1/folders/"+jsonval.Text(folder["id"])+"/files", headers, []Param{{"per_page", "100"}}, false)
		all = append(all, folderFiles...)
	}
	return all
}

func addError(e ErrorEntry) {
	mu.Lock()
	defer mu.Unlock()
	stats.Errors = append(stats.Errors, e)
}

// processCourse downloads one course's files and updates the statistics.
func processCourse(ctx context.Context, canvasURL string, headers map[string]string, course map[string]any) {
	courseID := jsonval.Text(course["id"])
	courseName := SanitizeFilename(jsonval.Text(get(course, "name", "Course_"+courseID)))
	courseCode := jsonval.Text(get(course, "course_code", ""))
	coursePath := filepath.Join(DownloadRoot, courseName)
	if jsonval.Truthy(get(course, "course_code", "")) {
		coursePath = filepath.Join(DownloadRoot, courseCode+"_"+courseName)
	}
	os.MkdirAll(coursePath, 0o755)
	printf("Processing course: %s\n", courseName)

	modulesCount, fromModules, fromFiles, failedCount := 0, 0, 0, 0

	// 1. Files referenced in Modules
	modules, _ := FetchAllPages(ctx, canvasURL+"/api/v1/courses/"+courseID+"/modules", headers,
		[]Param{{"include[]", "items"}, {"per_page", "100"}}, false)
	for _, module := range modules {
		moduleName := SanitizeFilename(jsonval.Text(get(module, "name", "Module_"+jsonval.Text(module["id"]))))
		modulePath := filepath.Join(coursePath, "Modules", moduleName)
		modulesCount++
		// Gather module items when they were not pre-included
		var items []map[string]any
		if raw, ok := module["items"].([]any); ok {
			for _, item := range raw {
				if m, ok := item.(map[string]any); ok {
					items = append(items, m)
				}
			}
		}
		if len(items) == 0 {
			items, _ = FetchAllPages(ctx, canvasURL+"/api/v1/courses/"+courseID+"/modules/"+jsonval.Text(module["id"])+"/items",
				headers, []Param{{"per_page", "100"}}, false)
		}
		for _, item := range items {
			if item["type"] != "File" || !jsonval.Truthy(item["content_id"]) {
				continue
			}
			info := fileInfo(ctx, canvasURL, headers, jsonval.Text(item["content_id"]))
			if info == nil {
				continue
			}
			fileName := SanitizeFilename(jsonval.Text(get(info, "display_name", "unnamed")))
			if ok, msg := downloadFile(ctx, info, filepath.Join(modulePath, fileName)); ok {
				fromModules++
			} else {
				failedCount++
				addError(ErrorEntry{Course: courseName, Module: moduleName, File: fileName, Error: msg})
			}
		}
	}

	// 2. Files from the Files area (they live under a dedicated folder)
	for _, info := range courseFiles(ctx, canvasURL, headers, courseID) {
		fileName := SanitizeFilename(jsonval.Text(get(info, "display_name", "unnamed")))
		if ok, msg := downloadFile(ctx, info, filepath.Join(coursePath, "Files", fileName)); ok {
			fromFiles++
		} else {
			failedCount++
			addError(ErrorEntry{Course: courseName, File: fileName, Error: msg})
		}
	}

	mu.Lock()
	stats.Courses++
	stats.Modules += modulesCount
	stats.FilesTotal += fromModules + fromFiles
	stats.FilesFailed += failedCount
	mu.Unlock()
}

func canUploadToVectorStore(path string, info os.FileInfo) bool {
	return supportedExtensions[strings.ToLower(filepath.Ext(path))] && info.Size() <= MaxFileSize
}

func uploadToVectorStore(ctx context.Context, api openai.Client, vectorStoreID, path, courseName string) (bool, string) {
	fileID, err := func() (string, error) {
		f, err := os.Open(path)
		if err != nil {
			return "", err
		}
		defer f.Close()
		uploaded, err := api.Files.New(ctx, openai.FileNewParams{
			File: openai.File(f, filepath.Base(path), ""), Purpose: openai.FilePurposeAssistants})
		if err != nil {
			return "", err
		}
		// Attach the uploaded file to the Vector Store
		_, err = api.VectorStores.Files.New(ctx, vectorStoreID, openai.VectorStoreFileNewParams{FileID: uploaded.ID})
		return uploaded.ID, err
	}()
	mu.Lock()
	defer mu.Unlock()
	if err != nil {
		stats.FilesUploadFailed++
		stats.Errors = append(stats.Errors, ErrorEntry{Course: courseName, File: filepath.Base(path), Error: "Vector Store upload failed: " + err.Error()})
		return false, err.Error()
	}
	stats.FilesUploadedToVectorStore++
	return true, fileID
}

func createVectorStore(ctx context.Context, api openai.Client, courseName, courseCode string) string {
	name := courseName
	if courseCode != "" {
		name = courseCode + "_" + courseName
	}
	store, err := api.VectorStores.New(ctx, openai.VectorStoreNewParams{Name: openai.String(textutil.Truncate(name, 100))})
	if err != nil {
		printf("❌ Failed to create Vector Store: %v\n", err)
		return ""
	}
	mu.Lock()
	stats.VectorStoresCreated++
	mu.Unlock()
	return store.ID
}

// Main drives the download workflow and the optional Vector Store upload. skipDownload only uploads files
// that were downloaded before.
func Main(ctx context.Context, skipDownload bool) {
	line := strings.Repeat("=", 70)
	printf("\n%s\n", line)
	if skipDownload {
		printf("☁️  Canvas file upload to Vector Store\n")
	} else {
		printf("📦 Canvas full file index downloader + Vector Store upload\n")
	}
	printf("%s\n\n", line)

	canvasURL, canvasToken := os.Getenv("CANVAS_URL"), os.Getenv("CANVAS_ACCESS_TOKEN")
	openaiKey := os.Getenv("OPENAI_API_KEY")
	if canvasURL == "" || canvasToken == "" {
		printf("❌ Error: Canvas configuration missing\nEnsure the .env file includes:\n  - CANVAS_URL\n  - CANVAS_ACCESS_TOKEN\n")
		return
	}
	absRoot, _ := filepath.Abs(DownloadRoot)
	printf("✓ Canvas URL: %s\n✓ Canvas token configured\n✓ Download directory: %s\n", canvasURL, absRoot)

	var api openai.Client
	upload := false
	if openaiKey != "" {
		api = models.AssistantsClient(openaiKey)
		printf("✓ OpenAI API configured\n")
		// Verify Vector Stores API access with a lightweight call
		if _, err := api.VectorStores.List(ctx, openai.VectorStoreListParams{Limit: openai.Int(1)}); err != nil {
			printf("⚠️  Vector Stores API test failed: %v\n   Continuing anyway; upload may still succeed\n", err)
		} else {
			printf("✓ Vector Stores API reachable\n")
		}
		upload = true
	} else {
		printf("⚠️  OPENAI_API_KEY missing; skipping Vector Store upload\n   Please set: OPENAI_API_KEY\n")
	}
	printf("\n")
	os.MkdirAll(DownloadRoot, 0o755)

	if skipDownload {
		entries, _ := os.ReadDir(DownloadRoot)
		if len(entries) == 0 {
			printf("❌ Error: the file_index directory is missing or empty\nRun the download mode first or populate: %s\n", absRoot)
			return
		}
		printf("✓ Located download folder: %s\n", absRoot)
	}
	start := time.Now()

	if !skipDownload {
		headers := map[string]string{"Authorization": "Bearer " + canvasToken, "Accept": "application/json"}
		printf("\n📚 Fetching course list...\n")
		courses, _ := FetchAllPages(ctx, canvasURL+"/api/v1/courses", headers, []Param{{"enrollment_state", "active"}, {"per_page", "100"}}, false)
		printf("✓ Located %d courses\n\n", len(courses))
		if len(courses) == 0 {
			printf("⚠️  No courses found\n")
			return
		}
		printf("📋 Course list:\n")
		for i, c := range courses {
			printf("  %d. %s (ID: %s)\n", i+1, jsonval.Text(get(c, "name", "N/A")), jsonval.Text(c["id"]))
		}
		printf("\n")
		courses = selectCourses(courses)
		if len(courses) == 0 {
			printf("⚠️  No courses selected for download\n")
			return
		}
		printf("Downloading all files for %d course(s)\n", len(courses))
		mu.Lock()
		auto, confirm := autoMode, autoConfirm
		mu.Unlock()
		response := "n"
		if auto {
			if confirm {
				response = "y"
			}
		} else {
			response = readLine("Continue? (y/n): ")
		}
		if strings.ToLower(response) != "y" {
			printf("Operation cancelled\n")
			return
		}
		printf("\n")
		for i, course := range courses {
			processCourse(ctx, canvasURL, headers, course)
			printf("Overall progress: %d/%d\n", i+1, len(courses))
		}
	} else {
		printf("⏩ Skipping downloads, processing existing files\n\n")
	}

	if upload {
		printf("\n%s\n☁️  Uploading files to the OpenAI Vector Store\n%s\n\n", line, line)
		courseFiles := map[string][]string{}
		var courseNames []string
		entries, _ := os.ReadDir(DownloadRoot)
		for _, entry := range entries {
			if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			var files []string
			filepath.Walk(filepath.Join(DownloadRoot, entry.Name()), func(path string, info os.FileInfo, err error) error {
				if err == nil && info.Mode().IsRegular() && canUploadToVectorStore(path, info) {
					files = append(files, path)
				}
				return nil
			})
			if len(files) > 0 {
				courseFiles[entry.Name()] = files
				courseNames = append(courseNames, entry.Name())
			}
		}
		if len(courseFiles) > 0 {
			total := 0
			for _, files := range courseFiles {
				total += len(files)
			}
			printf("Identified %d course folder(s) with %d uploadable file(s)\n\n", len(courseFiles), total)
			type storeInfo struct {
				VectorStoreID string              `json:"vector_store_id"`
				Files         []map[string]string `json:"files"`
			}
			mapping := map[string]*storeInfo{}
			for _, courseName := range courseNames {
				printf("Processing: %s\n", textutil.Substr(courseName, 0, 40))
				// Create one Vector Store per course
				storeID := createVectorStore(ctx, api, courseName, "")
				if storeID == "" {
					continue
				}
				mapping[courseName] = &storeInfo{VectorStoreID: storeID, Files: []map[string]string{}}
				for _, path := range courseFiles[courseName] {
					if ok, fileID := uploadToVectorStore(ctx, api, storeID, path, courseName); ok {
						rel, _ := filepath.Rel(DownloadRoot, path)
						mapping[courseName].Files = append(mapping[courseName].Files, map[string]string{"path": rel, "file_id": fileID})
					}
					time.Sleep(100 * time.Millisecond) // respect rate limits
				}
			}
			mappingPath := filepath.Join(DownloadRoot, "vector_stores_mapping.json")
			data, _ := json.MarshalIndent(mapping, "", "  ")
			os.WriteFile(mappingPath, data, 0o644)
			printf("\n✓ Saved Vector Store mapping to: %s\n", mappingPath)
		} else {
			printf("⚠️  No files qualified for Vector Store upload\n")
		}
	}

	duration := time.Since(start).Seconds()
	s := CurrentStats()
	printf("\n%s\n✅ Download phase complete!\n%s\n\n", line, line)
	printf("Download and Upload Summary\n")
	row := func(k, v string) { printf("  %-30s %15s\n", k, v) }
	row("━━━━ Download stats ━━━━", "")
	row("Courses processed", fmt.Sprint(s.Courses))
	row("Modules processed", fmt.Sprint(s.Modules))
	row("Files discovered", fmt.Sprint(s.FilesTotal))
	row("Files downloaded", fmt.Sprint(s.FilesDownloaded))
	row("Files skipped (existing)", fmt.Sprint(s.FilesSkipped))
	row("Download failures", fmt.Sprint(s.FilesFailed))
	row("Total size", fmt.Sprintf("%.2f MB", float64(s.TotalSize)/(1024*1024)))
	if upload {
		row("━━━━ Vector Store ━━━━", "")
		row("Vector Stores created", fmt.Sprint(s.VectorStoresCreated))
		row("Files uploaded", fmt.Sprint(s.FilesUploadedToVectorStore))
		row("Upload failures", fmt.Sprint(s.FilesUploadFailed))
	}
	row("━━━━━━━━━━━━", "")
	row("Total duration", fmt.Sprintf("%.1f s", duration))
	printf("\n")

	report := map[string]any{
		"timestamp": time.Now().Format("2006-01-02T15:04:05.000000"), "canvas_url": canvasURL,
		"statistics": s, "duration_seconds": duration,
	}
	reportPath := filepath.Join(DownloadRoot, "download_report.json")
	data, _ := json.MarshalIndent(report, "", "  ")
	os.WriteFile(reportPath, data, 0o644)
	printf("📄 Saved report: %s\n", reportPath)

	if len(s.Errors) > 0 {
		printf("\n⚠️  %d file(s) failed during download:\n", len(s.Errors))
		for i, e := range s.Errors {
			if i >= 20 { // Limit display to first 20 entries
				break
			}
			printf("  %s | %s | %s\n", e.Course, e.File, textutil.Substr(e.Error, 0, 50))
		}
		if len(s.Errors) > 20 {
			printf("\n... %d additional errors omitted\n", len(s.Errors)-20)
		}
	}
	printf("\n📁 Files stored at: %s\n", absRoot)
}
