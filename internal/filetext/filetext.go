// Package filetext extracts readable text from course files so the agent can work with their contents, not
// just link to them.
//
// Supported: PDF (per page), Word .docx (paragraphs and tables), PowerPoint .pptx (per slide, with notes),
// Excel .xlsx (per sheet), old .doc/.ppt/.xls (via catdoc), Jupyter notebooks, and text-like files (txt, md,
// csv, code, HTML, ...). Anything else (images, video, archives, scanned PDFs without a text layer) returns no
// text and a note saying why, so the agent can tell the student instead of guessing.
package filetext

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ledongthuc/pdf"
	"golang.org/x/net/html"

	"canvas_go/internal/jsonval"
	"canvas_go/internal/textutil"
)

const MaxSheetRows = 2000

var textExtensions = setOf(".txt", ".md", ".markdown", ".csv", ".tsv", ".json", ".xml", ".yaml", ".yml", ".tex", ".bib", ".rtf",
	".py", ".java", ".c", ".h", ".cpp", ".hpp", ".cc", ".js", ".ts", ".r", ".m", ".sql", ".sh", ".go", ".rs",
	".rb", ".php", ".swift", ".kt", ".scala", ".hs", ".lisp", ".asm", ".s", ".v", ".sv", ".vhd", ".ml", ".jl")

var htmlExtensions = setOf(".html", ".htm", ".xhtml")

// Old binary Office formats, read with the catdoc package's tools (installed in the Docker image)
var legacyOffice = map[string][]string{
	".doc": {"catdoc", "-w", "-d", "utf-8"},
	".ppt": {"catppt", "-d", "utf-8"},
	".xls": {"xls2csv", "-d", "utf-8"},
}

func setOf(items ...string) map[string]bool {
	out := map[string]bool{}
	for _, item := range items {
		out[item] = true
	}
	return out
}

// readError is a broken file: (text, note) = ("", "The file couldn't be read (...)").
type readError struct {
	kind string
	msg  string
}

func (e *readError) Error() string { return e.msg }

// Suffix is the file name's extension including the dot ("" for none or a dotfile).
func Suffix(name string) string {
	base := filepath.Base(name)
	if base == "." || base == "/" {
		return ""
	}
	i := strings.LastIndex(base, ".")
	if i <= 0 || i == len(base)-1 {
		return ""
	}
	return base[i:]
}

// ExtractText returns (text, note). text is empty when nothing readable was found; note explains why.
func ExtractText(data []byte, name, contentType string) (text, note string) {
	ext := strings.ToLower(Suffix(name))
	contentType = strings.ToLower(contentType)
	result, handled, err := func() (r [2]string, handled bool, err error) {
		defer func() {
			if p := recover(); p != nil {
				handled, err = true, &readError{"Error", fmt.Sprint(p)}
			}
		}()
		// The file name and the file's own signature decide; the declared content type is only a fallback
		switch {
		case ext == ".pdf" || bytes.HasPrefix(data, []byte("%PDF-")) || (ext == "" && contentType == "application/pdf"):
			t, n, err := pdfText(data)
			return [2]string{t, n}, true, err
		case ext == ".docx":
			t, err := docxText(data)
			return [2]string{t, ""}, true, err
		case ext == ".pptx":
			t, err := pptxText(data)
			return [2]string{t, ""}, true, err
		case ext == ".xlsx" || ext == ".xlsm":
			t, n, err := xlsxText(data)
			return [2]string{t, n}, true, err
		case ext == ".ipynb":
			t, err := notebookText(data)
			return [2]string{t, ""}, true, err
		case htmlExtensions[ext] || contentType == "text/html":
			return [2]string{HTMLToText(textutil.DecodeUTF8(data)), ""}, true, nil
		case textExtensions[ext] || strings.HasPrefix(contentType, "text/"):
			return [2]string{textutil.DecodeUTF8(data), ""}, true, nil
		}
		return r, false, nil
	}()
	if err != nil { // a broken file must not break the agent
		kind := "Error"
		if re, ok := err.(*readError); ok {
			kind = re.kind
		}
		return "", fmt.Sprintf("The file couldn't be read (%s: %s).", kind, textutil.Substr(err.Error(), 0, 200))
	}
	if handled {
		return result[0], result[1]
	}
	if _, ok := legacyOffice[ext]; ok {
		return legacyOfficeText(data, ext)
	}
	kind := contentType
	if kind == "" {
		kind = "unknown"
		if ext != "" {
			kind = ext[1:]
		}
	}
	return "", fmt.Sprintf("This is a %s file, which has no readable text (images, video, audio and archives aren't read).", kind)
}

var (
	pageMarkerRE = regexp.MustCompile(`--- Page \d+ ---`)
	wordCharRE   = regexp.MustCompile(`[\p{L}\p{N}_]`)
)

func pdfText(data []byte) (string, string, error) {
	reader, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", "", &readError{"PdfReadError", err.Error()}
	}
	count := reader.NumPage()
	pages := make([]string, 0, count)
	for number := 1; number <= count; number++ {
		page := reader.Page(number)
		text := ""
		if !page.V.IsNull() {
			text = pagePlainText(page)
		}
		pages = append(pages, fmt.Sprintf("--- Page %d ---\n%s", number, strings.TrimSpace(text)))
	}
	text := strings.Join(pages, "\n\n")
	if !wordCharRE.MatchString(pageMarkerRE.ReplaceAllString(text, "")) {
		return "", fmt.Sprintf("This PDF has %d page(s) but no text layer (probably scanned images), so its text can't be read.", count), nil
	}
	return text, "", nil
}

// pagePlainText reads a page's text line by line (rows of text at the same height), falling back to the
// library's plain-text extraction.
func pagePlainText(page pdf.Page) (out string) {
	defer func() {
		if recover() != nil {
			out = ""
		}
	}()
	rows, err := page.GetTextByRow()
	if err == nil && len(rows) > 0 {
		lines := make([]string, 0, len(rows))
		for _, row := range rows {
			var b strings.Builder
			for _, word := range row.Content {
				b.WriteString(word.S)
			}
			lines = append(lines, b.String())
		}
		return strings.Join(lines, "\n")
	}
	text, _ := page.GetPlainText(nil)
	return text
}

func legacyOfficeText(data []byte, ext string) (string, string) {
	command := legacyOffice[ext]
	if _, err := exec.LookPath(command[0]); err != nil {
		return "", fmt.Sprintf("This is an old %s Office file, and the tool that reads those (%s) isn't installed.", ext, command[0])
	}
	tmp, err := os.MkdirTemp("", "filetext-")
	if err != nil {
		return "", fmt.Sprintf("This %s file couldn't be read (it may be damaged or not a real %s file).", ext, ext)
	}
	defer os.RemoveAll(tmp)
	path := filepath.Join(tmp, "file"+ext)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Sprintf("This %s file couldn't be read (it may be damaged or not a real %s file).", ext, ext)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, command[0], append(command[1:], path)...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	runErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Sprintf("Reading this %s file took too long.", ext)
	}
	text := strings.TrimSpace(textutil.DecodeUTF8(stdout.Bytes()))
	if runErr != nil || text == "" {
		return "", fmt.Sprintf("This %s file couldn't be read (it may be damaged or not a real %s file).", ext, ext)
	}
	return text, ""
}

func notebookText(data []byte) (string, error) {
	var notebook map[string]any
	if err := json.Unmarshal([]byte(textutil.DecodeUTF8(data)), &notebook); err != nil {
		return "", &readError{"JSONDecodeError", err.Error()}
	}
	cells, _ := notebook["cells"].([]any)
	var out []string
	for number, raw := range cells {
		cell, _ := raw.(map[string]any)
		var source strings.Builder
		switch s := cell["source"].(type) {
		case string:
			source.WriteString(s)
		case []any:
			for _, line := range s {
				source.WriteString(jsonval.Text(line))
			}
		}
		cellType := "?"
		if t, ok := cell["cell_type"]; ok {
			cellType = jsonval.Text(t)
		}
		out = append(out, fmt.Sprintf("--- Cell %d (%s) ---\n%s", number+1, cellType, source.String()))
	}
	return strings.Join(out, "\n\n"), nil
}

var blankLinesRE = regexp.MustCompile(`\n{3,}`)

var blockTags = setOf("p", "br", "div", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6")

// HTMLToText keeps the text of an HTML document: scripts and styles dropped, block tags start new lines.
func HTMLToText(doc string) string {
	var parts strings.Builder
	skip := 0
	tokens := html.NewTokenizer(strings.NewReader(doc))
	for {
		switch tokens.Next() {
		case html.ErrorToken:
			return finishHTML(parts.String())
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := tokens.TagName()
			switch tag := string(name); {
			case tag == "script" || tag == "style":
				skip++
			case blockTags[tag]:
				parts.WriteString("\n")
			}
		case html.EndTagToken:
			if name, _ := tokens.TagName(); (string(name) == "script" || string(name) == "style") && skip > 0 {
				skip--
			}
		case html.TextToken:
			if skip == 0 {
				parts.Write(tokens.Text())
			}
		}
	}
}

func finishHTML(text string) string {
	return strings.TrimSpace(blankLinesRE.ReplaceAllString(text, "\n\n"))
}
