package canvastools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"

	"canvas_go/internal/filetext"
	"canvas_go/internal/jsonval"
	"canvas_go/internal/sandbox"
	"canvas_go/internal/textutil"
	"canvas_go/internal/tools"
)

func failed(what string, err error) (any, error) {
	return tools.Failf("Failed to %s: %v", what, err), nil
}

// --- courses -----------------------------------------------------------------------------------------------

// ListCourses lists all of the student's courses.
type ListCourses struct{ base }

func NewListCourses() *ListCourses {
	return &ListCourses{spec("canvas_list_courses",
		"Get the list of all courses the current student is enrolled in, including each course's name, ID, status, and other details",
		nil,
		optional(str("enrollment_state", "Enrollment state: active or completed (default: active)")),
		optional(str("include", "Extra information to include; options: total_students, teachers, syllabus_body")),
	)}
}

func (t *ListCourses) Forward(ctx context.Context, args tools.Args) (any, error) {
	params := []Param{{"enrollment_state", args.Str("enrollment_state", "active")}, {"per_page", "50"}}
	if include := args.Str("include", ""); include != "" {
		params = append(params, Param{"include[]", include})
	}
	result := MakeRequest(ctx, "GET", "courses", params, nil)
	if msg, isErr := apiError(result); isErr {
		return tools.Fail(msg), nil
	}
	courses, err := list(result)
	if err != nil {
		return failed("get courses", err)
	}
	lines := make([]string, len(courses))
	for i, c := range courses {
		lines[i] = fmt.Sprintf("- [%s] %s (%s)", field(c, "id", "unknown"), field(c, "name", "unknown"), field(c, "course_code", "unknown"))
	}
	return tools.OK(fmt.Sprintf("Found %d courses:\n", len(courses)) + strings.Join(lines, "\n")), nil
}

// --- assignments -------------------------------------------------------------------------------------------

// GetAssignments lists a course's assignments.
type GetAssignments struct{ base }

func NewGetAssignments() *GetAssignments {
	return &GetAssignments{spec("canvas_get_assignments",
		"Get all assignments in a specified course, including each assignment's name, due date, points, and other details",
		[]string{"course_id"},
		str("course_id", "Course ID"),
		optional(tools.Property{Name: "include_submission", Type: "boolean", Description: "Whether to include submission information (default: true)"}),
	)}
}

func (t *GetAssignments) Forward(ctx context.Context, args tools.Args) (any, error) {
	courseID := args.Str("course_id", "")
	params := []Param{{"per_page", "50"}}
	if args.Bool("include_submission", true) {
		params = append(params, Param{"include[]", "submission"})
	}
	result := MakeRequest(ctx, "GET", "courses/"+courseID+"/assignments", params, nil)
	if msg, isErr := apiError(result); isErr {
		return tools.Fail(msg), nil
	}
	assignments, err := list(result)
	if err != nil {
		return failed("get assignments", err)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Course %s has %d assignments:\n", courseID, len(assignments))
	for _, a := range assignments {
		status := "unsubmitted"
		if submission := object(a, "submission"); len(submission) > 0 {
			status = field(submission, "workflow_state", "unsubmitted")
		}
		fmt.Fprintf(&out, "- [%s] %s (Due: %s, Points: %s, Status: %s)\n", field(a, "id", "unknown"), field(a, "name", "unknown"),
			field(a, "due_at", "no due date"), field(a, "points_possible", "0"), status)
	}
	return tools.OK(out.String()), nil
}

// SubmitAssignment submits an assignment (never reaches live Canvas: blocked, or recorded in sandbox).
type SubmitAssignment struct{ base }

func NewSubmitAssignment() *SubmitAssignment {
	return &SubmitAssignment{spec("canvas_submit_assignment",
		"Submit an assignment in a course, as text or a URL."+WritePolicy,
		[]string{"course_id", "assignment_id", "submission_type"},
		str("course_id", "Course ID"),
		str("assignment_id", "Assignment ID"),
		str("submission_type", "Submission type: online_text_entry, online_url, or online_upload"),
		optional(str("body", "Submission text (used with online_text_entry)")),
		optional(str("url", "URL to submit (used with online_url)")),
	)}
}

func (t *SubmitAssignment) Forward(ctx context.Context, args tools.Args) (any, error) {
	courseID, assignmentID := args.Str("course_id", ""), args.Str("assignment_id", "")
	submissionType := args.Str("submission_type", "")
	submission := map[string]any{"submission_type": submissionType}
	if body := args.Str("body", ""); submissionType == "online_text_entry" && body != "" {
		submission["body"] = body
	} else if u := args.Str("url", ""); submissionType == "online_url" && u != "" {
		submission["url"] = u
	}
	result := MakeRequest(ctx, "POST", fmt.Sprintf("courses/%s/assignments/%s/submissions", courseID, assignmentID), nil,
		map[string]any{"submission": submission})
	if msg, isErr := apiError(result); isErr {
		return tools.Fail(msg), nil
	}
	m, ok := result.(map[string]any)
	if !ok {
		return failed("submit assignment", errUnexpected("an object"))
	}
	out := fmt.Sprintf("Assignment submitted successfully!\n- Assignment ID: %s\n- Submission type: %s\n- Submitted at: %s",
		assignmentID, submissionType, field(m, "submitted_at", "unknown"))
	if jsonval.Truthy(m["sandbox"]) {
		out += "\n- SANDBOX: " + field(m, "note", "unknown")
	}
	return tools.OK(out), nil
}

// --- modules -----------------------------------------------------------------------------------------------

// GetModules lists a course's modules.
type GetModules struct{ base }

func NewGetModules() *GetModules {
	return &GetModules{spec("canvas_get_modules", "Get all modules and the content structure of a specified course",
		[]string{"course_id"}, str("course_id", "Course ID"))}
}

func (t *GetModules) Forward(ctx context.Context, args tools.Args) (any, error) {
	courseID := args.Str("course_id", "")
	result := MakeRequest(ctx, "GET", "courses/"+courseID+"/modules", []Param{{"per_page", "50"}}, nil)
	if msg, isErr := apiError(result); isErr {
		return tools.Fail(msg), nil
	}
	modules, err := list(result)
	if err != nil {
		return failed("get modules", err)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Module structure of course %s:\n", courseID)
	for _, m := range modules {
		fmt.Fprintf(&out, "\n📚 Module [%s]: %s\n", field(m, "id", "unknown"), field(m, "name", "unknown"))
		fmt.Fprintf(&out, "   Status: %s\n", field(m, "workflow_state", "unknown"))
		fmt.Fprintf(&out, "   Items: %s\n", field(m, "items_count", "0"))
	}
	return tools.OK(out.String()), nil
}

// GetModuleItems lists the items in a module.
type GetModuleItems struct{ base }

func NewGetModuleItems() *GetModuleItems {
	return &GetModuleItems{spec("canvas_get_module_items",
		"Get all content items (files, pages, assignments, etc.) in a specified module",
		[]string{"course_id", "module_id"}, str("course_id", "Course ID"), str("module_id", "Module ID"))}
}

var moduleIcons = map[string]string{
	"Assignment": "📝", "Page": "📄", "File": "📁", "Discussion": "💬", "Quiz": "✏️", "ExternalUrl": "🔗", "ExternalTool": "🔧",
}

func (t *GetModuleItems) Forward(ctx context.Context, args tools.Args) (any, error) {
	courseID, moduleID := args.Str("course_id", ""), args.Str("module_id", "")
	result := MakeRequest(ctx, "GET", fmt.Sprintf("courses/%s/modules/%s/items", courseID, moduleID), []Param{{"per_page", "50"}}, nil)
	if msg, isErr := apiError(result); isErr {
		return tools.Fail(msg), nil
	}
	items, err := list(result)
	if err != nil {
		return failed("get module items", err)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Contents of module %s:\n", moduleID)
	for _, item := range items {
		typ, _ := item["type"].(string)
		icon, ok := moduleIcons[typ]
		if !ok {
			icon = "•"
		}
		fmt.Fprintf(&out, "%s [%s] %s (%s)\n", icon, field(item, "id", "unknown"), field(item, "title", "unknown"), field(item, "type", "unknown"))
		if typ == "File" && jsonval.Truthy(item["content_id"]) {
			// The file's own id, so its details and download link can be fetched with canvas_get_file_info
			fmt.Fprintf(&out, "   File ID: %s\n", field(item, "content_id", "unknown"))
		} else if typ == "Page" && jsonval.Truthy(item["page_url"]) {
			// The page's address, which canvas_get_page_content needs (titles are not addresses)
			fmt.Fprintf(&out, "   Page URL: %s\n", field(item, "page_url", "unknown"))
		}
	}
	return tools.OK(out.String()), nil
}

// --- files -------------------------------------------------------------------------------------------------

// GetFiles lists a course's files.
type GetFiles struct{ base }

func NewGetFiles() *GetFiles {
	return &GetFiles{spec("canvas_get_files", "Get all files and resources in a specified course",
		[]string{"course_id"}, str("course_id", "Course ID"), optional(str("search_term", "Search keyword (optional)")))}
}

func (t *GetFiles) Forward(ctx context.Context, args tools.Args) (any, error) {
	courseID := args.Str("course_id", "")
	params := []Param{{"per_page", "50"}}
	if term := args.Str("search_term", ""); term != "" {
		params = append(params, Param{"search_term", term})
	}
	result := MakeRequest(ctx, "GET", "courses/"+courseID+"/files", params, nil)
	if msg, isErr := apiError(result); isErr {
		return tools.Fail(msg), nil
	}
	files, err := list(result)
	if err != nil {
		return failed("get files", err)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Files in course %s:\n", courseID)
	for _, f := range files {
		sizeMB := number(f, "size") / (1024 * 1024)
		fmt.Fprintf(&out, "📁 [%s] %s ", field(f, "id", "unknown"), field(f, "display_name", "unknown"))
		fmt.Fprintf(&out, "(%.2fMB, %s)\n", sizeMB, field(f, "content-type", "unknown type"))
		fmt.Fprintf(&out, "   URL: %s\n", field(f, "url", "unknown"))
	}
	return tools.OK(out.String()), nil
}

// GetFileInfo shows a file's details and download link.
type GetFileInfo struct{ base }

func NewGetFileInfo() *GetFileInfo {
	return &GetFileInfo{spec("canvas_get_file_info",
		"Get detailed information about a specified file, including download link, size, type, etc.",
		[]string{"file_id"}, str("file_id", "File ID"))}
}

func (t *GetFileInfo) Forward(ctx context.Context, args tools.Args) (any, error) {
	result := MakeRequest(ctx, "GET", "files/"+args.Str("file_id", ""), nil, nil)
	if msg, isErr := apiError(result); isErr {
		return tools.Fail(msg), nil
	}
	f, ok := result.(map[string]any)
	if !ok {
		return failed("get file information", errUnexpected("an object"))
	}
	sizeMB := number(f, "size") / (1024 * 1024)
	var out strings.Builder
	out.WriteString("📁 File information:\n")
	fmt.Fprintf(&out, "Name: %s\n", field(f, "display_name", "unknown"))
	fmt.Fprintf(&out, "ID: %s\n", field(f, "id", "unknown"))
	fmt.Fprintf(&out, "Size: %.2f MB\n", sizeMB)
	fmt.Fprintf(&out, "Type: %s\n", field(f, "content-type", "unknown"))
	fmt.Fprintf(&out, "Modified at: %s\n", field(f, "modified_at", "unknown"))
	fmt.Fprintf(&out, "Download link: %s\n", field(f, "url", "unknown"))
	return tools.OK(out.String()), nil
}

// ReadFile reads the text content of a course file.
type ReadFile struct{ base }

const (
	ReadChunkChars = 12000
	ReadMaxBytes   = 60 * 1024 * 1024
)

type cachedText struct{ name, text, note string }

// TextCache keeps the text of the 16 most recently read files (file id -> name, text, note).
var TextCache, _ = lru.New[string, cachedText](16)

func NewReadFile() *ReadFile {
	return &ReadFile{spec("canvas_read_file",
		"Read the text content of a course file (PDF, Word, PowerPoint, Excel, text, code or notebook) so you "+
			"can work with what it says, e.g. a worksheet's questions, an assignment sheet or lecture notes. Long "+
			"files come in parts: call again with `start` to continue. Images and scanned PDFs have no readable text.",
		[]string{"file_id"},
		str("file_id", "File ID (from file lists, file search, or module items)"),
		optional(tools.Property{Name: "start", Type: "integer", Description: "Character offset to continue reading from (default 0)"}),
	)}
}

func (t *ReadFile) Forward(ctx context.Context, args tools.Args) (any, error) {
	fileID := args.Str("file_id", "")
	result := MakeRequest(ctx, "GET", "files/"+fileID, nil, nil)
	if msg, isErr := apiError(result); isErr {
		return tools.Fail(msg), nil
	}
	meta, ok := result.(map[string]any)
	if !ok {
		return failed("read file", errUnexpected("an object"))
	}
	name := "file " + fileID
	if jsonval.Truthy(meta["display_name"]) {
		name = jsonval.Text(meta["display_name"])
	} else if jsonval.Truthy(meta["filename"]) {
		name = jsonval.Text(meta["filename"])
	}
	cached, ok := TextCache.Get(fileID)
	if !ok {
		data, problem, err := fileBytes(ctx, fileID, meta)
		if err != nil {
			return failed("read file", err)
		}
		if problem != "" {
			return tools.OK(fmt.Sprintf("📄 %s: %s", name, problem)), nil
		}
		contentType, _ := meta["content-type"].(string)
		text, note := filetext.ExtractText(data, name, contentType)
		cached = cachedText{name, text, note}
		TextCache.Add(fileID, cached)
	}
	name, text, note := cached.name, cached.text, cached.note
	if strings.TrimSpace(text) == "" {
		if note == "" {
			note = "No readable text found."
		}
		return tools.OK(fmt.Sprintf("📄 %s: %s", name, note)), nil
	}
	start := max(0, args.Int("start", 0))
	total := textutil.Len(text)
	var out strings.Builder
	fmt.Fprintf(&out, "📄 %s (%d characters of text", name, total)
	if start > 0 {
		fmt.Fprintf(&out, ", showing from character %d)\n", start)
	} else {
		out.WriteString(")\n")
	}
	if note != "" {
		fmt.Fprintf(&out, "Note: %s\n", note)
	}
	out.WriteString(textutil.Substr(text, start, start+ReadChunkChars))
	if start+ReadChunkChars < total {
		fmt.Fprintf(&out, "\n\n[More text follows: call canvas_read_file with file_id=%s and start=%d to continue.]", fileID, start+ReadChunkChars)
	}
	return tools.OK(out.String()), nil
}

// fileBytes returns the file's contents, or a problem to tell the model.
func fileBytes(ctx context.Context, fileID string, meta map[string]any) ([]byte, string, error) {
	if size, ok := jsonval.Float(meta["size"]); ok && size > ReadMaxBytes {
		return nil, fmt.Sprintf("The file is too large to read here (%.0f MB).", size/(1024*1024)), nil
	}
	if sandbox.ActiveURL() != "" {
		// Sandbox mode: read the downloaded copy; nothing is fetched from Canvas
		path := sandbox.LocalFilePath(fileID)
		if path == "" {
			return nil, "This file isn't in the sandbox (Canvas didn't allow it to be downloaded).", nil
		}
		data, err := os.ReadFile(path)
		return data, "", err
	}
	fileURL, _ := meta["url"].(string)
	if fileURL == "" || jsonval.Truthy(meta["locked_for_user"]) {
		return nil, "This file is locked or has no download link in Canvas.", nil
	}
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	// No auth header: Canvas download URLs are pre-signed and redirect to file storage
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Sprintf("Downloading the file failed (HTTP %d).", resp.StatusCode), nil
	}
	data, err := io.ReadAll(resp.Body)
	return data, "", err
}

// GetFolders lists a course's folders.
type GetFolders struct{ base }

func NewGetFolders() *GetFolders {
	return &GetFolders{spec("canvas_get_folders", "Get the folder structure of a specified course",
		[]string{"course_id"}, str("course_id", "Course ID"))}
}

func (t *GetFolders) Forward(ctx context.Context, args tools.Args) (any, error) {
	courseID := args.Str("course_id", "")
	result := MakeRequest(ctx, "GET", "courses/"+courseID+"/folders", []Param{{"per_page", "50"}}, nil)
	if msg, isErr := apiError(result); isErr {
		return tools.Fail(msg), nil
	}
	folders, err := list(result)
	if err != nil {
		return failed("get folders", err)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "📂 Folders in course %s:\n", courseID)
	for _, f := range folders {
		fmt.Fprintf(&out, "• [%s] %s\n", field(f, "id", "unknown"), field(f, "full_name", "unknown"))
		fmt.Fprintf(&out, "  Files: %s\n", field(f, "files_count", "0"))
	}
	return tools.OK(out.String()), nil
}

// GetFolderFiles lists the files in a folder.
type GetFolderFiles struct{ base }

func NewGetFolderFiles() *GetFolderFiles {
	return &GetFolderFiles{spec("canvas_get_folder_files", "Get all files in a specified folder",
		[]string{"folder_id"}, str("folder_id", "Folder ID"))}
}

func (t *GetFolderFiles) Forward(ctx context.Context, args tools.Args) (any, error) {
	folderID := args.Str("folder_id", "")
	result := MakeRequest(ctx, "GET", "folders/"+folderID+"/files", []Param{{"per_page", "50"}}, nil)
	if msg, isErr := apiError(result); isErr {
		return tools.Fail(msg), nil
	}
	files, err := list(result)
	if err != nil {
		return failed("get folder files", err)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "📂 Files in folder %s:\n", folderID)
	for _, f := range files {
		sizeMB := number(f, "size") / (1024 * 1024)
		fmt.Fprintf(&out, "📄 [%s] %s\n", field(f, "id", "unknown"), field(f, "display_name", "unknown"))
		fmt.Fprintf(&out, "   Size: %.2f MB\n", sizeMB)
		fmt.Fprintf(&out, "   Type: %s\n", field(f, "content-type", "unknown"))
	}
	return tools.OK(out.String()), nil
}

// SearchFiles searches a course's files.
type SearchFiles struct{ base }

func NewSearchFiles() *SearchFiles {
	return &SearchFiles{spec("canvas_search_files", "Search for files in a specified course",
		[]string{"course_id", "search_term"}, str("course_id", "Course ID"), str("search_term", "Search keyword"))}
}

func (t *SearchFiles) Forward(ctx context.Context, args tools.Args) (any, error) {
	courseID, term := args.Str("course_id", ""), args.Str("search_term", "")
	result := MakeRequest(ctx, "GET", "courses/"+courseID+"/files", []Param{{"search_term", term}, {"per_page", "50"}}, nil)
	if msg, isErr := apiError(result); isErr {
		return tools.Fail(msg), nil
	}
	files, err := list(result)
	if err != nil {
		return failed("search files", err)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "🔍 Results for '%s':\n", term)
	if len(files) == 0 {
		out.WriteString("No matching files found")
	}
	for _, f := range files {
		sizeKB := number(f, "size") / (1024)
		fmt.Fprintf(&out, "\n📄 %s\n", field(f, "display_name", "unknown"))
		fmt.Fprintf(&out, "   File ID: %s\n", field(f, "id", "unknown"))
		fmt.Fprintf(&out, "   Size: %.2f KB\n", sizeKB)
		fmt.Fprintf(&out, "   Download: %s\n", field(f, "url", "unknown"))
	}
	return tools.OK(out.String()), nil
}

// --- discussions and announcements -------------------------------------------------------------------------

// GetDiscussions lists a course's discussion topics.
type GetDiscussions struct{ base }

func NewGetDiscussions() *GetDiscussions {
	return &GetDiscussions{spec("canvas_get_discussions", "Get all discussion topics in a specified course",
		[]string{"course_id"}, str("course_id", "Course ID"))}
}

func (t *GetDiscussions) Forward(ctx context.Context, args tools.Args) (any, error) {
	courseID := args.Str("course_id", "")
	result := MakeRequest(ctx, "GET", "courses/"+courseID+"/discussion_topics", []Param{{"per_page", "50"}}, nil)
	if msg, isErr := apiError(result); isErr {
		return tools.Fail(msg), nil
	}
	topics, err := list(result)
	if err != nil {
		return failed("get discussions", err)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Discussions in course %s:\n", courseID)
	for _, topic := range topics {
		fmt.Fprintf(&out, "💬 [%s] %s\n", field(topic, "id", "unknown"), field(topic, "title", "unknown"))
		fmt.Fprintf(&out, "   Posted at: %s\n", field(topic, "posted_at", "unknown"))
		fmt.Fprintf(&out, "   Replies: %s\n", field(topic, "discussion_subentry_count", "0"))
	}
	return tools.OK(out.String()), nil
}

// PostDiscussion posts a discussion reply (never reaches live Canvas: blocked, or recorded in sandbox).
type PostDiscussion struct{ base }

func NewPostDiscussion() *PostDiscussion {
	return &PostDiscussion{spec("canvas_post_discussion",
		"Post a reply or comment in a specified discussion topic."+WritePolicy,
		[]string{"course_id", "topic_id", "message"},
		str("course_id", "Course ID"), str("topic_id", "Discussion topic ID"), str("message", "Message to post"))}
}

func (t *PostDiscussion) Forward(ctx context.Context, args tools.Args) (any, error) {
	courseID, topicID := args.Str("course_id", ""), args.Str("topic_id", "")
	result := MakeRequest(ctx, "POST", fmt.Sprintf("courses/%s/discussion_topics/%s/entries", courseID, topicID), nil,
		map[string]any{"message": args.Str("message", "")})
	if msg, isErr := apiError(result); isErr {
		return tools.Fail(msg), nil
	}
	m, ok := result.(map[string]any)
	if !ok {
		return failed("post discussion reply", errUnexpected("an object"))
	}
	out := fmt.Sprintf("Discussion reply posted successfully!\n- Topic ID: %s\n- Posted at: %s", topicID, field(m, "created_at", "unknown"))
	if jsonval.Truthy(m["sandbox"]) {
		out += "\n- SANDBOX: " + field(m, "note", "unknown")
	}
	return tools.OK(out), nil
}

// GetAnnouncements lists the latest announcements.
type GetAnnouncements struct{ base }

func NewGetAnnouncements() *GetAnnouncements {
	return &GetAnnouncements{spec("canvas_get_announcements",
		"Get the latest announcements for all courses (or only the courses given in context_codes)", nil,
		optional(str("context_codes", "Comma-separated course context codes, format: course_123,course_456 (optional; leave empty to get all courses)")))}
}

func (t *GetAnnouncements) Forward(ctx context.Context, args tools.Args) (any, error) {
	params := []Param{{"per_page", "20"}}
	if codes := args.Str("context_codes", ""); codes != "" {
		for _, code := range strings.Split(codes, ",") {
			params = append(params, Param{"context_codes[]", code})
		}
	}
	result := MakeRequest(ctx, "GET", "announcements", params, nil)
	if msg, isErr := apiError(result); isErr {
		return tools.Fail(msg), nil
	}
	announcements, err := list(result)
	if err != nil {
		return failed("get announcements", err)
	}
	var out strings.Builder
	out.WriteString("📢 Latest announcements:\n")
	for _, a := range announcements {
		content := textutil.Truncate(field(a, "message", "no content"), 200)
		fmt.Fprintf(&out, "\nTitle: %s\n", field(a, "title", "unknown"))
		fmt.Fprintf(&out, "Posted at: %s\n", field(a, "posted_at", "unknown"))
		fmt.Fprintf(&out, "Content: %s...\n", content)
	}
	return tools.OK(out.String()), nil
}

// --- calendar, grades, pages, quizzes ----------------------------------------------------------------------

// GetCalendarEvents lists calendar events.
type GetCalendarEvents struct{ base }

func NewGetCalendarEvents() *GetCalendarEvents {
	return &GetCalendarEvents{spec("canvas_get_calendar_events",
		"Get the student's calendar events, including course activities, assignment due dates, etc.", nil,
		optional(str("start_date", "Start date, format: YYYY-MM-DD (optional)")),
		optional(str("end_date", "End date, format: YYYY-MM-DD (optional)")))}
}

func (t *GetCalendarEvents) Forward(ctx context.Context, args tools.Args) (any, error) {
	params := []Param{{"per_page", "50"}}
	if start := args.Str("start_date", ""); start != "" {
		params = append(params, Param{"start_date", start})
	}
	if end := args.Str("end_date", ""); end != "" {
		params = append(params, Param{"end_date", end})
	}
	result := MakeRequest(ctx, "GET", "calendar_events", params, nil)
	if msg, isErr := apiError(result); isErr {
		return tools.Fail(msg), nil
	}
	events, err := list(result)
	if err != nil {
		return failed("get calendar events", err)
	}
	var out strings.Builder
	out.WriteString("📅 Calendar events:\n")
	for _, e := range events {
		fmt.Fprintf(&out, "\n🗓️ %s\n", field(e, "title", "unknown"))
		fmt.Fprintf(&out, "   Time: %s\n", field(e, "start_at", "unknown"))
		fmt.Fprintf(&out, "   Type: %s\n", field(e, "type", "unknown"))
		if jsonval.Truthy(e["description"]) {
			description := textutil.Truncate(field(e, "description", ""), 100)
			fmt.Fprintf(&out, "   Description: %s...\n", description)
		}
	}
	return tools.OK(out.String()), nil
}

// GetGrades shows the student's grades in a course.
type GetGrades struct{ base }

func NewGetGrades() *GetGrades {
	return &GetGrades{spec("canvas_get_grades", "Get the student's grade information in a specified course",
		[]string{"course_id"}, str("course_id", "Course ID"))}
}

func (t *GetGrades) Forward(ctx context.Context, args tools.Args) (any, error) {
	courseID := args.Str("course_id", "")
	// Get the current user ID
	user := MakeRequest(ctx, "GET", "users/self", nil, nil)
	if msg, isErr := apiError(user); isErr {
		return tools.Fail(msg), nil
	}
	userMap, _ := user.(map[string]any)
	// Get enrollment info (includes grades)
	result := MakeRequest(ctx, "GET", "courses/"+courseID+"/enrollments", []Param{{"user_id", jsonval.Text(userMap["id"])}}, nil)
	if msg, isErr := apiError(result); isErr {
		return tools.Fail(msg), nil
	}
	enrollments, err := list(result)
	if err != nil {
		return failed("get grades", err)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Grades in course %s:\n", courseID)
	for _, e := range enrollments {
		grades := object(e, "grades")
		fmt.Fprintf(&out, "📊 Current grade: %s\n", field(grades, "current_grade", "N/A"))
		fmt.Fprintf(&out, "   Current score: %s\n", field(grades, "current_score", "N/A"))
		fmt.Fprintf(&out, "   Final grade: %s\n", field(grades, "final_grade", "N/A"))
		fmt.Fprintf(&out, "   Final score: %s\n", field(grades, "final_score", "N/A"))
	}
	return tools.OK(out.String()), nil
}

// GetPages lists a course's pages.
type GetPages struct{ base }

func NewGetPages() *GetPages {
	return &GetPages{spec("canvas_get_pages", "Get all pages (wiki pages) in a specified course",
		[]string{"course_id"}, str("course_id", "Course ID"))}
}

func (t *GetPages) Forward(ctx context.Context, args tools.Args) (any, error) {
	courseID := args.Str("course_id", "")
	result := MakeRequest(ctx, "GET", "courses/"+courseID+"/pages", []Param{{"per_page", "50"}}, nil)
	if msg, isErr := apiError(result); isErr {
		return tools.Fail(msg), nil
	}
	pages, err := list(result)
	if err != nil {
		return failed("get pages", err)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Pages in course %s:\n", courseID)
	for _, p := range pages {
		fmt.Fprintf(&out, "📄 %s\n", field(p, "title", "unknown"))
		fmt.Fprintf(&out, "   URL: %s\n", field(p, "url", "unknown"))
		fmt.Fprintf(&out, "   Updated at: %s\n", field(p, "updated_at", "unknown"))
	}
	return tools.OK(out.String()), nil
}

// GetPageContent shows a page's full content.
type GetPageContent struct{ base }

func NewGetPageContent() *GetPageContent {
	return &GetPageContent{spec("canvas_get_page_content", "Get the full content of a specified page",
		[]string{"course_id", "page_url"}, str("course_id", "Course ID"), str("page_url", "Page URL or title"))}
}

func (t *GetPageContent) Forward(ctx context.Context, args tools.Args) (any, error) {
	result := MakeRequest(ctx, "GET", fmt.Sprintf("courses/%s/pages/%s", args.Str("course_id", ""), args.Str("page_url", "")), nil, nil)
	if msg, isErr := apiError(result); isErr {
		return tools.Fail(msg), nil
	}
	page, ok := result.(map[string]any)
	if !ok {
		return failed("get page content", errUnexpected("an object"))
	}
	out := fmt.Sprintf("📄 Page: %s\nUpdated at: %s\n\nContent:\n%s\n", field(page, "title", "unknown"), field(page, "updated_at", "unknown"),
		field(page, "body", "no content"))
	return tools.OK(out), nil
}

// GetQuizzes lists a course's quizzes.
type GetQuizzes struct{ base }

func NewGetQuizzes() *GetQuizzes {
	return &GetQuizzes{spec("canvas_get_quizzes", "Get all quizzes in a specified course",
		[]string{"course_id"}, str("course_id", "Course ID"))}
}

func (t *GetQuizzes) Forward(ctx context.Context, args tools.Args) (any, error) {
	courseID := args.Str("course_id", "")
	result := MakeRequest(ctx, "GET", "courses/"+courseID+"/quizzes", []Param{{"per_page", "50"}}, nil)
	if msg, isErr := apiError(result); isErr {
		return tools.Fail(msg), nil
	}
	quizzes, err := list(result)
	if err != nil {
		return failed("get quizzes", err)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Quizzes in course %s:\n", courseID)
	for _, q := range quizzes {
		fmt.Fprintf(&out, "✏️ [%s] %s\n", field(q, "id", "unknown"), field(q, "title", "unknown"))
		fmt.Fprintf(&out, "   Type: %s\n", field(q, "quiz_type", "unknown"))
		fmt.Fprintf(&out, "   Points: %s\n", field(q, "points_possible", "0"))
		fmt.Fprintf(&out, "   Due: %s\n", field(q, "due_at", "no due date"))
	}
	return tools.OK(out.String()), nil
}

// --- the student's own lists -------------------------------------------------------------------------------

// GetTodoItems lists the student's to-dos.
type GetTodoItems struct{ base }

func NewGetTodoItems() *GetTodoItems {
	return &GetTodoItems{spec("canvas_get_todo_items", "Get the student's to-do list, including upcoming assignments and tasks", nil)}
}

func (t *GetTodoItems) Forward(ctx context.Context, args tools.Args) (any, error) {
	result := MakeRequest(ctx, "GET", "users/self/todo", nil, nil)
	if msg, isErr := apiError(result); isErr {
		return tools.Fail(msg), nil
	}
	items, err := list(result)
	if err != nil {
		return failed("get to-do items", err)
	}
	var out strings.Builder
	out.WriteString("📝 To-do items:\n")
	for _, item := range items {
		assignment := object(item, "assignment")
		fmt.Fprintf(&out, "\n• %s\n", field(assignment, "name", "unknown task"))
		fmt.Fprintf(&out, "  Course: %s\n", field(item, "context_name", "unknown"))
		fmt.Fprintf(&out, "  Due: %s\n", field(assignment, "due_at", "no due date"))
		fmt.Fprintf(&out, "  Points: %s\n", field(assignment, "points_possible", "0"))
	}
	return tools.OK(out.String()), nil
}

// GetUpcomingEvents lists upcoming events.
type GetUpcomingEvents struct{ base }

func NewGetUpcomingEvents() *GetUpcomingEvents {
	return &GetUpcomingEvents{spec("canvas_get_upcoming_events", "Get all of the student's upcoming events and activities", nil)}
}

func (t *GetUpcomingEvents) Forward(ctx context.Context, args tools.Args) (any, error) {
	result := MakeRequest(ctx, "GET", "users/self/upcoming_events", nil, nil)
	if msg, isErr := apiError(result); isErr {
		return tools.Fail(msg), nil
	}
	events, err := list(result)
	if err != nil {
		return failed("get upcoming events", err)
	}
	var out strings.Builder
	out.WriteString("🗓️ Upcoming events:\n")
	for _, e := range events {
		fmt.Fprintf(&out, "\n• %s\n", field(e, "title", "unknown event"))
		fmt.Fprintf(&out, "  Time: %s\n", field(e, "start_at", "unknown"))
		fmt.Fprintf(&out, "  Type: %s\n", field(e, "type", "unknown"))
	}
	return tools.OK(out.String()), nil
}

// GetGroups lists the student's groups.
type GetGroups struct{ base }

func NewGetGroups() *GetGroups {
	return &GetGroups{spec("canvas_get_groups", "Get all groups the student belongs to", nil)}
}

func (t *GetGroups) Forward(ctx context.Context, args tools.Args) (any, error) {
	result := MakeRequest(ctx, "GET", "users/self/groups", nil, nil)
	if msg, isErr := apiError(result); isErr {
		return tools.Fail(msg), nil
	}
	groups, err := list(result)
	if err != nil {
		return failed("get groups", err)
	}
	var out strings.Builder
	out.WriteString("👥 My groups:\n")
	for _, g := range groups {
		fmt.Fprintf(&out, "\n• [%s] %s\n", field(g, "id", "unknown"), field(g, "name", "unknown"))
		fmt.Fprintf(&out, "  Members: %s\n", field(g, "members_count", "0"))
		fmt.Fprintf(&out, "  Course: %s\n", field(g, "course_id", "unknown"))
	}
	return tools.OK(out.String()), nil
}
