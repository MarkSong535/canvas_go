package canvastools

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/openai/openai-go/v3"

	"canvas_go/internal/latexsvg"
	"canvas_go/internal/models"
	"canvas_go/internal/textutil"
	"canvas_go/internal/tools"
)

const noKey = "OPENAI_API_KEY is not configured; add it to the .env file"

func assistantsClient() (openai.Client, bool) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return openai.Client{}, false
	}
	return models.AssistantsClient(key), true
}

func localTime(ts int64) string { return time.Unix(ts, 0).Local().Format("2006-01-02 15:04:05") }

func rule() string { return strings.Repeat("=", 60) }

func sizeText(n int64) string {
	kb := float64(n) / 1024
	if kb >= 1024 {
		return fmt.Sprintf("%.2f MB", kb/1024)
	}
	return fmt.Sprintf("%.2f KB", kb)
}

// fileContent downloads a file's content from the Files API.
func fileContent(ctx context.Context, client openai.Client, fileID string) ([]byte, error) {
	resp, err := client.Files.Content(ctx, fileID)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// VectorStoreList lists the course knowledge bases.
type VectorStoreList struct{ base }

func NewVectorStoreList() *VectorStoreList {
	return &VectorStoreList{spec("vector_store_list",
		"List all available course knowledge bases (vector stores), showing each one's ID, name, and file count", nil)}
}

func (t *VectorStoreList) Forward(ctx context.Context, args tools.Args) (any, error) {
	client, ok := assistantsClient()
	if !ok {
		return tools.Fail(noKey), nil
	}
	page, err := client.VectorStores.List(ctx, openai.VectorStoreListParams{Limit: openai.Int(100)})
	if err != nil {
		return tools.Failf("Failed to get knowledge bases: %v", err), nil
	}
	if len(page.Data) == 0 {
		return tools.OK("📋 No knowledge bases are available\nRun file_index_downloader first to create one"), nil
	}
	var out strings.Builder
	fmt.Fprintf(&out, "📚 Found %d course knowledge bases:\n\n", len(page.Data))
	for i, vs := range page.Data {
		fmt.Fprintf(&out, "%d. [%s] %s\n", i+1, vs.ID, vs.Name)
		fmt.Fprintf(&out, "   📁 Files: %d\n", vs.FileCounts.Total)
		fmt.Fprintf(&out, "   📅 Created at: %d\n\n", vs.CreatedAt)
	}
	return tools.OK(out.String()), nil
}

// VectorStoreSearch searches a course knowledge base.
type VectorStoreSearch struct{ base }

func NewVectorStoreSearch() *VectorStoreSearch {
	return &VectorStoreSearch{spec("vector_store_search",
		"Search a specified course knowledge base for relevant content; use it to answer questions about course materials, lecture notes, assignments, etc.",
		[]string{"vector_store_id", "query"},
		str("vector_store_id", "Vector store ID (from the vector_store_list tool)"),
		str("query", "Search query, e.g. 'What is this course mainly about?' or 'Requirements for Assignment 1'"),
		optional(tools.Property{Name: "max_results", Type: "integer", Description: "Maximum number of results to return (default: 5)"}),
	)}
}

func (t *VectorStoreSearch) Forward(ctx context.Context, args tools.Args) (any, error) {
	client, ok := assistantsClient()
	if !ok {
		return tools.Fail(noKey), nil
	}
	query := args.Str("query", "")
	page, err := client.VectorStores.Search(ctx, args.Str("vector_store_id", ""), openai.VectorStoreSearchParams{
		Query:         openai.VectorStoreSearchParamsQueryUnion{OfString: openai.String(query)},
		MaxNumResults: openai.Int(int64(args.Int("max_results", 5))),
	})
	if err != nil {
		return tools.Failf("Search failed: %v", err), nil
	}
	if len(page.Data) == 0 {
		return tools.OK(fmt.Sprintf("🔍 Search query: \"%s\"\n❌ No relevant content found", query)), nil
	}
	var out strings.Builder
	fmt.Fprintf(&out, "🔍 Search query: \"%s\"\n", query)
	fmt.Fprintf(&out, "📊 Found %d relevant results:\n\n", len(page.Data))
	for i, result := range page.Data {
		out.WriteString(rule() + "\n")
		fmt.Fprintf(&out, "Result %d:\n", i+1)
		fmt.Fprintf(&out, "📈 Relevance: %.2f%%\n", result.Score*100)
		fmt.Fprintf(&out, "📄 Source: %s\n", result.Filename)
		if len(result.Attributes) > 0 {
			fmt.Fprintf(&out, "🏷️  Attributes: %s\n", result.JSON.Attributes.Raw())
		}
		texts := make([]string, len(result.Content))
		for j, part := range result.Content {
			texts[j] = part.Text
		}
		content := strings.Join(texts, "\n")
		if textutil.Len(content) > 800 {
			content = textutil.Truncate(content, 800) + "...\n(content truncated)"
		}
		fmt.Fprintf(&out, "\n📝 Content:\n%s\n\n", content)
	}
	return tools.OK(out.String()), nil
}

// VectorStoreListFiles lists the files in a knowledge base.
type VectorStoreListFiles struct{ base }

func NewVectorStoreListFiles() *VectorStoreListFiles {
	return &VectorStoreListFiles{spec("vector_store_list_files",
		"List all files in a specified knowledge base, showing each file's ID, name, status, and other details; can optionally read file contents",
		[]string{"vector_store_id"},
		str("vector_store_id", "Vector store ID (from the vector_store_list tool)"),
		optional(tools.Property{Name: "read_content", Type: "boolean", Description: "Whether to read file contents (default: false, which only lists file information)"}),
		optional(tools.Property{Name: "limit", Type: "integer", Description: "Maximum number of files to return (default: 100; 0 or a negative number returns all files)"}),
	)}
}

func (t *VectorStoreListFiles) Forward(ctx context.Context, args tools.Args) (any, error) {
	client, ok := assistantsClient()
	if !ok {
		return tools.Fail(noKey), nil
	}
	storeID := args.Str("vector_store_id", "")
	readContent := args.Bool("read_content", false)
	limit := args.Int("limit", 100)
	var files []openai.VectorStoreFile
	if limit <= 0 {
		// Every file, 100 per page
		pager := client.VectorStores.Files.ListAutoPaging(ctx, storeID, openai.VectorStoreFileListParams{Limit: openai.Int(100)})
		for pager.Next() {
			files = append(files, pager.Current())
		}
		if err := pager.Err(); err != nil {
			return tools.Failf("Failed to get files: %v", err), nil
		}
	} else {
		page, err := client.VectorStores.Files.List(ctx, storeID, openai.VectorStoreFileListParams{Limit: openai.Int(int64(limit))})
		if err != nil {
			return tools.Failf("Failed to get files: %v", err), nil
		}
		files = page.Data
	}
	if len(files) == 0 {
		return tools.OK(fmt.Sprintf("📋 Vector Store [%s] has no files", storeID)), nil
	}
	var out strings.Builder
	fmt.Fprintf(&out, "📚 Vector Store [%s] files:\n", storeID)
	fmt.Fprintf(&out, "Found %d files\n\n", len(files))
	for i, file := range files {
		out.WriteString(rule() + "\n")
		fmt.Fprintf(&out, "File %d:\n", i+1)
		fmt.Fprintf(&out, "🆔 File ID: %s\n", file.ID)
		emoji := "⏳"
		if file.Status == "completed" {
			emoji = "✅"
		}
		fmt.Fprintf(&out, "%s Status: %s\n", emoji, file.Status)
		fmt.Fprintf(&out, "📅 Created at: %s\n", localTime(file.CreatedAt))
		// Always get basic file info (name, size, etc.)
		info, err := client.Files.Get(ctx, file.ID)
		if err != nil {
			fmt.Fprintf(&out, "\n⚠️  Failed to get file information: %v\n\n", err)
			continue
		}
		fmt.Fprintf(&out, "📄 File name: %s\n", info.Filename)
		fmt.Fprintf(&out, "📦 Size: %s\n", sizeText(info.Bytes))
		fmt.Fprintf(&out, "🎯 Purpose: %s\n", info.Purpose)
		if readContent && file.Status == "completed" {
			content, err := fileContent(ctx, client, file.ID)
			if err != nil {
				fmt.Fprintf(&out, "\n⚠️  Failed to read content: %v\n", err)
			} else if !utf8.Valid(content) {
				out.WriteString("\n⚠️  Cannot display file content (not a text file, or an encoding issue)\n")
				fmt.Fprintf(&out, "   File size: %d bytes\n", len(content))
			} else {
				text := string(content)
				if textutil.Len(text) > 1000 {
					text = textutil.Truncate(text, 1000) + "\n...(content truncated)"
				}
				fmt.Fprintf(&out, "\n📝 Content preview:\n%s\n", text)
			}
		}
		out.WriteString("\n")
	}
	return tools.OK(out.String()), nil
}

// VectorStoreGetFile shows a file's details and content.
type VectorStoreGetFile struct{ base }

func NewVectorStoreGetFile() *VectorStoreGetFile {
	return &VectorStoreGetFile{spec("vector_store_get_file",
		"Get a file's details by file_id and read its content; shows the full content of text files",
		[]string{"file_id"},
		str("file_id", "OpenAI file ID (from the vector_store_list_files tool)"),
		optional(tools.Property{Name: "max_length", Type: "integer", Description: "Maximum number of characters to display (default: 5000)"}),
	)}
}

func (t *VectorStoreGetFile) Forward(ctx context.Context, args tools.Args) (any, error) {
	client, ok := assistantsClient()
	if !ok {
		return tools.Fail(noKey), nil
	}
	fileID := args.Str("file_id", "")
	maxLength := args.Int("max_length", 5000)
	info, err := client.Files.Get(ctx, fileID)
	if err != nil {
		return tools.Failf("Failed to get file: %v", err), nil
	}
	var out strings.Builder
	out.WriteString("📄 File details:\n" + rule() + "\n")
	fmt.Fprintf(&out, "🆔 File ID: %s\n", info.ID)
	fmt.Fprintf(&out, "📝 File name: %s\n", info.Filename)
	fmt.Fprintf(&out, "🎯 Purpose: %s\n", info.Purpose)
	fmt.Fprintf(&out, "📦 Size: %s\n", sizeText(info.Bytes))
	fmt.Fprintf(&out, "📅 Created at: %s\n", localTime(info.CreatedAt))
	if info.Status != "" {
		emoji := "⏳"
		if info.Status == "processed" {
			emoji = "✅"
		}
		fmt.Fprintf(&out, "%s Status: %s\n", emoji, info.Status)
	}
	fmt.Fprintf(&out, "\n%s\n", rule())
	content, err := fileContent(ctx, client, fileID)
	switch {
	case err != nil:
		fmt.Fprintf(&out, "\n⚠️  Failed to read file content: %v\n", err)
	case !utf8.Valid(content):
		out.WriteString("\n⚠️  File is binary and cannot be displayed as text\n")
		fmt.Fprintf(&out, "File size: %d bytes\n", len(content))
		switch {
		case strings.HasPrefix(string(content), "%PDF"):
			out.WriteString("File type: PDF document\n")
		case strings.HasPrefix(string(content), "PK"):
			out.WriteString("File type: ZIP/Office document\n")
		default:
			out.WriteString("File type: unknown binary file\n")
		}
	default:
		text := string(content)
		total := textutil.Len(text)
		out.WriteString("\n📖 File content:\n" + rule() + "\n")
		if total > maxLength {
			out.WriteString(textutil.Truncate(text, maxLength))
			fmt.Fprintf(&out, "\n\n%s\n", rule())
			fmt.Fprintf(&out, "⚠️  Content truncated (showing %d/%d characters)\n", maxLength, total)
			fmt.Fprintf(&out, "Full content is %d characters\n", total)
		} else {
			out.WriteString(text)
			fmt.Fprintf(&out, "\n%s\n", rule())
			fmt.Fprintf(&out, "✅ Showing the full content (%d characters)\n", total)
		}
	}
	return tools.OK(out.String()), nil
}

// CheckLatex compiles LaTeX the way the student's screen will render it.
type CheckLatex struct{ base }

func NewCheckLatex() *CheckLatex {
	return &CheckLatex{spec("check_latex",
		"Compile a LaTeX math expression or a chemfig structure exactly the way it will be rendered for the "+
			"student, and report whether it renders or the LaTeX error. Use it to check complex formulas, "+
			"multi-line derivations, or chemfig structures before putting them in the final answer; simple math "+
			"does not need checking.",
		[]string{"latex"},
		str("latex", `The LaTeX without delimiters, e.g. '\frac{a}{b}' or '\chemfig{*6(=-=-=-)}'`),
		optional(str("kind", "'inline' or 'display' for math, 'chemfig' for a chemfig block (default 'display')")),
	)}
}

func (t *CheckLatex) Forward(ctx context.Context, args tools.Args) (any, error) {
	kind := args.Str("kind", "display")
	if kind != "inline" && kind != "display" && kind != "chemfig" {
		kind = "display"
	}
	if !latexsvg.Available() {
		return tools.OK("The renderer is not available here; the student's browser will render math itself."), nil
	}
	result := latexsvg.RenderMany(ctx, []latexsvg.Snippet{{Kind: kind, Tex: args.Str("latex", "")}})[0]
	if result.SVG != "" {
		return tools.OK("OK: this compiles and will render correctly."), nil
	}
	return tools.OK(fmt.Sprintf("Does not compile: %s. Fix the LaTeX and check again, or simplify it.", strings.TrimRight(result.Error, "."))), nil
}
