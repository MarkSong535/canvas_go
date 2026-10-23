package filetext

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"

	"canvas_go/internal/jsonval"
)

// node is a minimal XML element tree (local names only, namespaces dropped).
type node struct {
	Name     string
	Attrs    map[string]string // by local name
	Children []*node
	Text     string // character data directly inside this element
}

func parseXML(data []byte) (*node, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	root := &node{Name: "#document"}
	stack := []*node{root}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &node{Name: t.Name.Local, Attrs: map[string]string{}}
			for _, a := range t.Attr {
				n.Attrs[a.Name.Local] = a.Value
			}
			parent := stack[len(stack)-1]
			parent.Children = append(parent.Children, n)
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			stack[len(stack)-1].Text += string(t)
		}
	}
	return root, nil
}

func (n *node) child(name string) *node {
	for _, c := range n.Children {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func (n *node) children(name string) []*node {
	var out []*node
	for _, c := range n.Children {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

// find returns the first descendant with the given name path, e.g. find("body") or find("cSld", "spTree").
func (n *node) find(names ...string) *node {
	cur := n
	for _, name := range names {
		next := cur.descendant(name)
		if next == nil {
			return nil
		}
		cur = next
	}
	return cur
}

func (n *node) descendant(name string) *node {
	for _, c := range n.Children {
		if c.Name == name {
			return c
		}
		if d := c.descendant(name); d != nil {
			return d
		}
	}
	return nil
}

func (n *node) all(name string, out *[]*node) {
	for _, c := range n.Children {
		if c.Name == name {
			*out = append(*out, c)
		}
		c.all(name, out)
	}
}

type zipFiles map[string]*zip.File

func openZip(data []byte) (zipFiles, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, &readError{"BadZipFile", "File is not a zip file"}
	}
	out := zipFiles{}
	for _, f := range r.File {
		out[f.Name] = f
	}
	return out, nil
}

func (z zipFiles) read(name string) ([]byte, error) {
	f, ok := z[name]
	if !ok {
		return nil, &readError{"KeyError", fmt.Sprintf("There is no item named '%s' in the archive", name)}
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func (z zipFiles) xml(name string) (*node, error) {
	data, err := z.read(name)
	if err != nil {
		return nil, err
	}
	return parseXML(data)
}

// rels maps relationship ids to (target path, type) for a part, e.g. "ppt/slides/slide1.xml".
func (z zipFiles) rels(part string) map[string][2]string {
	dir, file := path.Split(part)
	root, err := z.xml(dir + "_rels/" + file + ".rels")
	out := map[string][2]string{}
	if err != nil {
		return out
	}
	for _, rel := range root.find("Relationships").children("Relationship") {
		target := rel.Attrs["Target"]
		if !strings.HasPrefix(target, "/") {
			target = path.Clean(dir + target)
		} else {
			target = strings.TrimPrefix(target, "/")
		}
		out[rel.Attrs["Id"]] = [2]string{target, rel.Attrs["Type"]}
	}
	return out
}

// --- Word ---

// wordParagraphText is a Word paragraph's text: its runs' text, tabs and line breaks.
func wordParagraphText(p *node) string {
	var b strings.Builder
	var walk func(n *node)
	walk = func(n *node) {
		for _, c := range n.Children {
			switch c.Name {
			case "t":
				b.WriteString(c.Text)
			case "tab":
				if n.Name == "r" {
					b.WriteString("\t")
				}
			case "br", "cr":
				b.WriteString("\n")
			case "r", "hyperlink", "ins", "smartTag", "fldSimple":
				walk(c)
			}
		}
	}
	walk(p)
	return b.String()
}

func docxText(data []byte) (string, error) {
	z, err := openZip(data)
	if err != nil {
		return "", err
	}
	doc, err := z.xml("word/document.xml")
	if err != nil {
		return "", err
	}
	body := doc.find("document", "body")
	if body == nil {
		return "", nil
	}
	var parts []string
	for _, p := range body.children("p") {
		if text := wordParagraphText(p); strings.TrimSpace(text) != "" {
			parts = append(parts, text)
		}
	}
	for number, table := range body.children("tbl") {
		parts = append(parts, fmt.Sprintf("--- Table %d ---", number+1))
		for _, row := range table.children("tr") {
			var cells []string
			for _, cell := range row.children("tc") {
				var paragraphs []string
				for _, p := range cell.children("p") {
					paragraphs = append(paragraphs, wordParagraphText(p))
				}
				text := strings.TrimSpace(strings.Join(paragraphs, "\n"))
				span := 1
				if props := cell.child("tcPr"); props != nil {
					if g := props.child("gridSpan"); g != nil {
						if n, err := strconv.Atoi(g.Attrs["val"]); err == nil && n > 1 {
							span = n
						}
					}
				}
				for i := 0; i < span; i++ {
					cells = append(cells, text)
				}
			}
			parts = append(parts, strings.Join(cells, " | "))
		}
	}
	return strings.Join(parts, "\n"), nil
}

// --- PowerPoint ---

// drawingText is a text frame's text: paragraphs and line breaks both become "\n".
func drawingText(txBody *node) string {
	var paragraphs []string
	for _, p := range txBody.children("p") {
		var b strings.Builder
		for _, c := range p.Children {
			switch c.Name {
			case "r", "fld":
				if t := c.child("t"); t != nil {
					b.WriteString(t.Text)
				}
			case "br":
				b.WriteString("\n")
			}
		}
		paragraphs = append(paragraphs, b.String())
	}
	return strings.Join(paragraphs, "\n")
}

func pptxText(data []byte) (string, error) {
	z, err := openZip(data)
	if err != nil {
		return "", err
	}
	presentation, err := z.xml("ppt/presentation.xml")
	if err != nil {
		return "", err
	}
	presRels := z.rels("ppt/presentation.xml")
	var slides []string
	number := 0
	if list := presentation.find("sldIdLst"); list != nil {
		for _, sldID := range list.children("sldId") {
			target, ok := presRels[sldID.Attrs["id"]]
			if !ok {
				continue
			}
			number++
			slide, err := z.xml(target[0])
			if err != nil {
				return "", err
			}
			lines := []string{fmt.Sprintf("--- Slide %d ---", number)}
			if tree := slide.find("cSld", "spTree"); tree != nil {
				for _, shape := range tree.Children {
					switch shape.Name {
					case "sp":
						if txBody := shape.child("txBody"); txBody != nil {
							if text := strings.TrimSpace(drawingText(txBody)); text != "" {
								lines = append(lines, text)
							}
						}
					case "graphicFrame":
						if table := shape.find("graphic", "graphicData", "tbl"); table != nil {
							for _, row := range table.children("tr") {
								var cells []string
								for _, cell := range row.children("tc") {
									text := ""
									if txBody := cell.child("txBody"); txBody != nil {
										text = strings.TrimSpace(drawingText(txBody))
									}
									cells = append(cells, text)
								}
								lines = append(lines, strings.Join(cells, " | "))
							}
						}
					}
				}
			}
			for _, rel := range z.rels(target[0]) {
				if !strings.HasSuffix(rel[1], "/notesSlide") {
					continue
				}
				if notes, err := z.xml(rel[0]); err == nil {
					if text := strings.TrimSpace(notesText(notes)); text != "" {
						lines = append(lines, "Notes: "+text)
					}
				}
			}
			slides = append(slides, strings.Join(lines, "\n"))
		}
	}
	return strings.Join(slides, "\n\n"), nil
}

// notesText is the text of the notes placeholder (the body placeholder) of a notes slide.
func notesText(notes *node) string {
	tree := notes.find("cSld", "spTree")
	if tree == nil {
		return ""
	}
	for _, shape := range tree.children("sp") {
		var placeholders []*node
		shape.all("ph", &placeholders)
		for _, ph := range placeholders {
			if ph.Attrs["type"] == "body" {
				if txBody := shape.child("txBody"); txBody != nil {
					return drawingText(txBody)
				}
			}
		}
	}
	return ""
}

// --- Excel ---

func xlsxText(data []byte) (string, string, error) {
	z, err := openZip(data)
	if err != nil {
		return "", "", err
	}
	workbook, err := z.xml("xl/workbook.xml")
	if err != nil {
		return "", "", err
	}
	var shared []string
	if sst, err := z.xml("xl/sharedStrings.xml"); err == nil {
		if root := sst.find("sst"); root != nil {
			for _, si := range root.children("si") {
				var texts []*node
				si.all("t", &texts)
				var b strings.Builder
				for _, t := range texts {
					b.WriteString(t.Text)
				}
				shared = append(shared, b.String())
			}
		}
	}
	rels := z.rels("xl/workbook.xml")
	var sheets []string
	truncated := false
	if list := workbook.find("sheets"); list != nil {
		for _, sheet := range list.children("sheet") {
			target, ok := rels[sheet.Attrs["id"]]
			if !ok {
				continue
			}
			doc, err := z.xml(target[0])
			if err != nil {
				return "", "", err
			}
			rows := []string{fmt.Sprintf("--- Sheet: %s ---", sheet.Attrs["name"])}
			grid, maxRow, maxCol := sheetCells(doc, shared)
			for r := 1; r <= maxRow; r++ {
				if r-1 >= MaxSheetRows {
					truncated = true
					break
				}
				cells := make([]string, maxCol)
				any := false
				for c := 1; c <= maxCol; c++ {
					if v, ok := grid[[2]int{r, c}]; ok {
						cells[c-1] = v
						any = true
					}
				}
				if any {
					rows = append(rows, strings.Join(cells, "\t"))
				}
			}
			sheets = append(sheets, strings.Join(rows, "\n"))
		}
	}
	note := ""
	if truncated {
		note = fmt.Sprintf("Only the first %d rows of each sheet are included.", MaxSheetRows)
	}
	return strings.Join(sheets, "\n\n"), note, nil
}

// sheetCells reads a worksheet's cell values as text, by (row, column).
func sheetCells(doc *node, shared []string) (map[[2]int]string, int, int) {
	grid := map[[2]int]string{}
	maxRow, maxCol := 0, 0
	if dim := doc.descendant("dimension"); dim != nil {
		ref := dim.Attrs["ref"]
		if i := strings.LastIndex(ref, ":"); i >= 0 {
			ref = ref[i+1:]
		}
		if r, c, ok := cellRef(ref); ok {
			maxRow, maxCol = r, c
		}
	}
	data := doc.descendant("sheetData")
	if data == nil {
		return grid, maxRow, maxCol
	}
	rowNumber := 0
	for _, row := range data.children("row") {
		if n, err := strconv.Atoi(row.Attrs["r"]); err == nil {
			rowNumber = n
		} else {
			rowNumber++
		}
		col := 0
		for _, c := range row.children("c") {
			r, cc, ok := cellRef(c.Attrs["r"])
			if ok {
				col = cc
			} else {
				r, col = rowNumber, col+1
			}
			value, has := cellValue(c, shared)
			if !has {
				continue
			}
			grid[[2]int{r, col}] = value
			maxRow, maxCol = max(maxRow, r), max(maxCol, col)
		}
	}
	return grid, maxRow, maxCol
}

func cellRef(ref string) (row, col int, ok bool) {
	i := 0
	for i < len(ref) && ref[i] >= 'A' && ref[i] <= 'Z' {
		col = col*26 + int(ref[i]-'A'+1)
		i++
	}
	row, err := strconv.Atoi(ref[i:])
	if i == 0 || err != nil {
		return 0, 0, false
	}
	return row, col, true
}

func cellValue(c *node, shared []string) (string, bool) {
	typ := c.Attrs["t"]
	if typ == "inlineStr" {
		var texts []*node
		c.all("t", &texts)
		var b strings.Builder
		for _, t := range texts {
			b.WriteString(t.Text)
		}
		return b.String(), true
	}
	v := c.child("v")
	if v == nil {
		return "", false
	}
	raw := v.Text
	switch typ {
	case "s":
		i, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || i < 0 || i >= len(shared) {
			return "", false
		}
		return shared[i], true
	case "b":
		if strings.TrimSpace(raw) == "1" {
			return "TRUE", true
		}
		return "FALSE", true
	case "str", "e":
		return raw, true
	}
	raw = strings.TrimSpace(raw)
	if !strings.ContainsAny(raw, ".eE") {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return strconv.FormatInt(n, 10), true
		}
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return jsonval.FormatFloat(f), true
	}
	return raw, true
}

// keep sort imported for deterministic helpers
var _ = sort.Strings
