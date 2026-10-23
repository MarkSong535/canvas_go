package tests

// Text extraction from course files (internal/filetext).

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"testing"

	"canvas_go/internal/filetext"
)

// makePDF is a minimal one-page PDF; with text it has a real text layer, without it the page is blank.
func makePDF(text string) []byte {
	var content []byte
	if text != "" {
		content = []byte(fmt.Sprintf("BT /F1 18 Tf 72 720 Td (%s) Tj ET", text))
	}
	objects := [][]byte{
		[]byte("<</Type/Catalog/Pages 2 0 R>>"),
		[]byte("<</Type/Pages/Kids[3 0 R]/Count 1>>"),
		[]byte("<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]/Contents 4 0 R/Resources<</Font<</F1 5 0 R>>>>>>"),
		append(append([]byte(fmt.Sprintf("<</Length %d>>stream\n", len(content))), content...), []byte("\nendstream")...),
		[]byte("<</Type/Font/Subtype/Type1/BaseFont/Helvetica>>"),
	}
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	var offsets []int
	for i, body := range objects {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n", i+1)
		out.Write(body)
		out.WriteString("\nendobj\n")
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer<</Size %d/Root 1 0 R>>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return out.Bytes()
}

func makeZip(files map[string]string) []byte {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, content := range files {
		f, _ := w.Create(name)
		f.Write([]byte(content))
	}
	w.Close()
	return buf.Bytes()
}

const wordNS = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"`

func wordParagraph(text string) string {
	return `<w:p><w:r><w:t xml:space="preserve">` + html.EscapeString(text) + `</w:t></w:r></w:p>`
}

// makeDOCX builds a Word document with paragraphs and an optional table.
func makeDOCX(paragraphs []string, table [][]string) []byte {
	var body strings.Builder
	for _, p := range paragraphs {
		body.WriteString(wordParagraph(p))
	}
	if table != nil {
		body.WriteString("<w:tbl>")
		for _, row := range table {
			body.WriteString("<w:tr>")
			for _, cell := range row {
				body.WriteString("<w:tc>" + wordParagraph(cell) + "</w:tc>")
			}
			body.WriteString("</w:tr>")
		}
		body.WriteString("</w:tbl>")
	}
	return makeZip(map[string]string{
		"[Content_Types].xml": `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`,
		"word/document.xml":   `<?xml version="1.0"?><w:document ` + wordNS + `><w:body>` + body.String() + `<w:sectPr/></w:body></w:document>`,
	})
}

const (
	pNS   = `xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"`
	relNS = `xmlns="http://schemas.openxmlformats.org/package/2006/relationships"`
)

func shape(placeholder, text string) string {
	body := "<a:p/>"
	if text != "" {
		body = "<a:p><a:r><a:t>" + html.EscapeString(text) + "</a:t></a:r></a:p>"
	}
	return `<p:sp><p:nvSpPr><p:cNvPr id="2" name="x"/><p:cNvSpPr/><p:nvPr><p:ph type="` + placeholder +
		`"/></p:nvPr></p:nvSpPr><p:spPr/><p:txBody><a:bodyPr/>` + body + `</p:txBody></p:sp>`
}

// makePPTX builds a one-slide deck: a title, an empty content placeholder, and speaker notes.
func makePPTX(title, notes string) []byte {
	return makeZip(map[string]string{
		"ppt/presentation.xml": `<?xml version="1.0"?><p:presentation ` + pNS + `><p:sldIdLst><p:sldId id="256" r:id="rId2"/></p:sldIdLst></p:presentation>`,
		"ppt/_rels/presentation.xml.rels": `<?xml version="1.0"?><Relationships ` + relNS + `><Relationship Id="rId2" ` +
			`Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="slides/slide1.xml"/></Relationships>`,
		"ppt/slides/slide1.xml": `<?xml version="1.0"?><p:sld ` + pNS + `><p:cSld><p:spTree>` + shape("title", title) + shape("body", "") +
			`</p:spTree></p:cSld></p:sld>`,
		"ppt/slides/_rels/slide1.xml.rels": `<?xml version="1.0"?><Relationships ` + relNS + `><Relationship Id="rId2" ` +
			`Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/notesSlide" Target="../notesSlides/notesSlide1.xml"/></Relationships>`,
		"ppt/notesSlides/notesSlide1.xml": `<?xml version="1.0"?><p:notes ` + pNS + `><p:cSld><p:spTree>` + shape("sldImg", "") + shape("body", notes) +
			`</p:spTree></p:cSld></p:notes>`,
	})
}

// makeXLSX builds a workbook with one sheet; strings go to the shared strings table, numbers stay numbers.
func makeXLSX(sheet string, rows [][]any) []byte {
	var shared []string
	var data strings.Builder
	for r, row := range rows {
		fmt.Fprintf(&data, `<row r="%d">`, r+1)
		for c, value := range row {
			ref := fmt.Sprintf("%c%d", 'A'+c, r+1)
			switch v := value.(type) {
			case string:
				fmt.Fprintf(&data, `<c r="%s" t="s"><v>%d</v></c>`, ref, len(shared))
				shared = append(shared, v)
			default:
				fmt.Fprintf(&data, `<c r="%s"><v>%v</v></c>`, ref, v)
			}
		}
		data.WriteString("</row>")
	}
	var sst strings.Builder
	for _, s := range shared {
		sst.WriteString("<si><t>" + html.EscapeString(s) + "</t></si>")
	}
	ns := `xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"`
	return makeZip(map[string]string{
		"xl/workbook.xml": `<?xml version="1.0"?><workbook ` + ns + `><sheets><sheet name="` + sheet + `" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<?xml version="1.0"?><Relationships ` + relNS + `><Relationship Id="rId1" ` +
			`Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/></Relationships>`,
		"xl/worksheets/sheet1.xml": `<?xml version="1.0"?><worksheet ` + ns + `><sheetData>` + data.String() + `</sheetData></worksheet>`,
		"xl/sharedStrings.xml":     `<?xml version="1.0"?><sst ` + ns + `>` + sst.String() + `</sst>`,
	})
}

func TestPDFTextByPage(t *testing.T) {
	text, note := filetext.ExtractText(makePDF("Question 1: Rewrite in active voice."), "worksheet.pdf", "")
	if !strings.Contains(text, "--- Page 1 ---") || !strings.Contains(text, "Question 1: Rewrite in active voice.") || note != "" {
		t.Fatalf("text %q note %q", text, note)
	}
}

func TestScannedPDFSaysWhy(t *testing.T) {
	text, note := filetext.ExtractText(makePDF(""), "scan.pdf", "")
	if text != "" || !strings.Contains(note, "no text layer") {
		t.Fatalf("text %q note %q", text, note)
	}
}

func TestDOCXParagraphsAndTables(t *testing.T) {
	data := makeDOCX([]string{"Master the Active Voice", "1. The ball was thrown by Sam."}, [][]string{{"Passive", "Active"}, {"was eaten", "ate"}})
	text, _ := filetext.ExtractText(data, "exercise.docx", "")
	if !strings.Contains(text, "1. The ball was thrown by Sam.") || !strings.Contains(text, "Passive | Active") {
		t.Fatalf("text %q", text)
	}
}

func TestPPTXSlidesAndNotes(t *testing.T) {
	text, _ := filetext.ExtractText(makePPTX("A* search", "Mention admissibility"), "lecture.pptx", "")
	for _, want := range []string{"--- Slide 1 ---", "A* search", "Notes: Mention admissibility"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text)
		}
	}
}

func TestXLSXSheets(t *testing.T) {
	data := makeXLSX("Grades", [][]any{{"Lab", "Score"}, {"Lab 1", 93.5}})
	text, _ := filetext.ExtractText(data, "grades.xlsx", "")
	if !strings.Contains(text, "--- Sheet: Grades ---") || !strings.Contains(text, "Lab 1\t93.5") {
		t.Fatalf("text %q", text)
	}
}

func TestTextHTMLAndNotebooks(t *testing.T) {
	if text, _ := filetext.ExtractText([]byte("int f(void) {\n    return 1;\n}\n"), "solution.c", ""); !strings.HasPrefix(text, "int f(void)") {
		t.Fatalf("code: %q", text)
	}
	text, _ := filetext.ExtractText([]byte("<html><style>x{}</style><p>Hello</p><p>World</p></html>"), "page.html", "")
	if !strings.Contains(text, "Hello") || !strings.Contains(text, "World") || strings.Contains(text, "x{}") {
		t.Fatalf("html: %q", text)
	}
	notebook, _ := json.Marshal(map[string]any{"cells": []any{map[string]any{"cell_type": "markdown", "source": []string{"# Lab 2"}}}})
	if text, _ := filetext.ExtractText(notebook, "lab2.ipynb", ""); !strings.Contains(text, "# Lab 2") {
		t.Fatalf("notebook: %q", text)
	}
}

func TestUnreadableTypesSayWhy(t *testing.T) {
	text, note := filetext.ExtractText([]byte("\x89PNG..."), "diagram.png", "image/png")
	_, emptyNote := filetext.ExtractText(nil, "x.png", "image/png")
	if text != "" || note != emptyNote || !strings.Contains(note, "no readable text") {
		t.Fatalf("png: %q %q", text, note)
	}
	text, note = filetext.ExtractText([]byte("\xd0\xcf"), "old.doc", "") // not a real .doc
	if text != "" || !strings.Contains(note, ".doc") || !(strings.Contains(note, "isn't installed") || strings.Contains(note, "couldn't be read")) {
		t.Fatalf("doc: %q %q", text, note)
	}
	if _, note := filetext.ExtractText([]byte("not a pdf"), "broken.pdf", ""); !strings.Contains(note, "couldn't be read") {
		t.Fatalf("broken pdf: %q", note)
	}
}
