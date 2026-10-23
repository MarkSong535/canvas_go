// Package latexsvg renders the LaTeX in agent answers to SVG on the server: inline math, display math, and
// ```chemfig structures.
//
// Pipeline: one LaTeX document per answer with one page per snippet -> DVI -> dvisvgm -> one SVG per page.
// Each snippet goes into a box whose width/height/depth is logged and used as the page size, so the SVG keeps
// the box's baseline: inline math is sized in em and vertically aligned like text. If the batch fails to
// compile, snippets are rendered one by one so a single bad formula doesn't take the others down.
//
// Needs `latex` (amsmath, mhchem, chemfig) and `dvisvgm` on PATH (installed in the Docker image). When they are
// missing nothing is rendered and front ends fall back to client-side KaTeX / the chemfig source.
//
// The LaTeX comes from model output, so it is treated as untrusted:
//   - LaTeX runs with shell escape disabled and paranoid file access (no absolute paths, no parent dirs, no
//     dotfiles) in an empty temporary directory, with a timeout.
//   - Snippets that use file I/O, raw specials, catcode/macro tricks or document-level commands are refused.
//   - The SVG is checked for active content here, and front ends sanitize it again before inserting it.
//
// Snippet keys ("I:<tex>", "D:<tex>", "C:<code>") use the exact text the answer contains, so front ends that
// extract math with the same rules can look renders up directly.
package latexsvg

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dlclark/regexp2"
	lru "github.com/hashicorp/golang-lru/v2"

	"canvas_go/internal/jsonval"
)

// Same extraction rules as the web UI: code is skipped; $$..$$ and \[..\] are display; \(..\) and $..$ inline
var (
	codeRE         = regexp.MustCompile("(```[\\s\\S]*?(?:```|$)|`[^`\\n]+`)")
	mathRE         = regexp2.MustCompile(`\$\$([\s\S]+?)\$\$|\\\[([\s\S]+?)\\\]|\\\(([\s\S]+?)\\\)|\$(?!\s)([^$\n]+?)(?<!\s)\$(?!\d)`, regexp2.None)
	chemfigBlockRE = regexp.MustCompile("(?s)```chemfig[ \\t]*\\n(.*?)```")
	forbiddenRE    = regexp2.MustCompile(`\\(?:input|include|includeonly|openin|openout|read|readline|write|message|immediate|newread|newwrite`+
		`|special|catcode|csname|def|edef|gdef|xdef|let|futurelet|expandafter|makeatletter|makeatother`+
		`|scantokens|everyeof|everyjob|endinput|directlua|shipout|usepackage|RequirePackage|documentclass`+
		`|loop|jobname|pgfsys\w*|tikzset|pgfkeys|hoffset|voffset|newcommand|renewcommand|providecommand)(?![A-Za-z])`+
		`|document\}`, regexp2.None) // `@` stays allowed (chemfig's @{name} atom labels); without \makeatletter it can't reach internal macros
	activeSVGRE = regexp.MustCompile(`(?i)<script|<foreignObject|\son\w+\s*=|javascript:`)
	blackRE     = regexp.MustCompile(`(fill|stroke)=(?:'(?:#000|#000000|black)'|"(?:#000|#000000|black)")`)
	boxRE       = regexp.MustCompile(`::SNIP (\d+) ([\d.]+)pt,([\d.]+)pt,([\d.]+)pt::`)
	errorRE     = regexp.MustCompile(`(?m)^! (.+)$`)
	pageRE      = regexp.MustCompile(`^page-0*(\d+)\.svg$`)
	widthRE     = regexp.MustCompile(`\swidth=(?:'[^'"]*'|"[^'"]*")`)
	heightRE    = regexp.MustCompile(`\sheight=(?:'[^'"]*'|"[^'"]*")`)
)

const (
	MaxSnippets = 40
	MaxTexChars = 4000
	Timeout     = 25 * time.Second
	CacheSize   = 1024
	PtPerEm     = 10.0 // LaTeX 10pt font: 1em of math == 1em of surrounding text
	PadPt       = 1.5  // room for ink that pokes out of the TeX box (italic overhangs, accents)
)

var scale = map[string]float64{"inline": 1.05, "display": 1.05, "chemfig": 1.25}

// Snippet is one piece of LaTeX from an answer.
type Snippet struct {
	Kind string // "inline" | "display" | "chemfig"
	Tex  string // exactly as written in the answer
}

// Key is how front ends look the render up.
func (s Snippet) Key() string {
	return map[string]string{"inline": "I", "display": "D", "chemfig": "C"}[s.Kind] + ":" + s.Tex
}

// Result is a render: SVG, or an error message.
type Result struct {
	SVG   string
	Error string
}

// Available reports whether latex and dvisvgm are installed.
func Available() bool {
	_, e1 := exec.LookPath("latex")
	_, e2 := exec.LookPath("dvisvgm")
	return e1 == nil && e2 == nil
}

// Extract returns all renderable snippets in an answer, in order, without duplicates.
func Extract(text string) []Snippet {
	var found []Snippet
	for _, m := range chemfigBlockRE.FindAllStringSubmatch(text, -1) {
		found = append(found, Snippet{"chemfig", strings.TrimSpace(m[1])})
	}
	for i, part := range splitKeepingCaptures(codeRE, text) {
		if i%2 == 1 { // code span / block
			continue
		}
		m, _ := mathRE.FindStringMatch(part)
		for m != nil {
			groups := m.Groups()
			d1, d2, i1, i2 := groups[1], groups[2], groups[3], groups[4]
			switch {
			case len(d1.Captures) > 0:
				found = append(found, Snippet{"display", d1.String()})
			case len(d2.Captures) > 0:
				found = append(found, Snippet{"display", d2.String()})
			case len(i1.Captures) > 0:
				found = append(found, Snippet{"inline", i1.String()})
			default:
				found = append(found, Snippet{"inline", i2.String()})
			}
			m, _ = mathRE.FindNextMatch(m)
		}
	}
	seen := map[Snippet]bool{}
	var unique []Snippet
	for _, s := range found {
		if strings.TrimSpace(s.Tex) == "" || seen[s] {
			continue
		}
		seen[s] = true
		unique = append(unique, s)
	}
	if len(unique) > MaxSnippets {
		unique = unique[:MaxSnippets]
	}
	return unique
}

// splitKeepingCaptures splits text around every match and keeps the matches: text, match, text, ..., text.
func splitKeepingCaptures(re *regexp.Regexp, text string) []string {
	var out []string
	last := 0
	for _, loc := range re.FindAllStringIndex(text, -1) {
		out = append(out, text[last:loc[0]], text[loc[0]:loc[1]])
		last = loc[1]
	}
	return append(out, text[last:])
}

// RenderAnswer renders every snippet in an answer: {snippet key: svg} for the ones that rendered.
func RenderAnswer(ctx context.Context, text string) map[string]string {
	out := map[string]string{}
	if !Available() {
		return out
	}
	snippets := Extract(text)
	for i, r := range RenderMany(ctx, snippets) {
		if r.SVG != "" {
			out[snippets[i].Key()] = r.SVG
		}
	}
	return out
}

var (
	cache, _  = lru.New[Snippet, Result](CacheSize)
	semaphore = make(chan struct{}, 2)
)

// RenderMany renders snippets; each result has an SVG or an error message.
func RenderMany(ctx context.Context, snippets []Snippet) []Result {
	results := map[Snippet]Result{}
	var todo []Snippet
	for _, s := range snippets {
		if r, ok := cache.Get(s); ok {
			results[s] = r
		} else if refusal := Refusal(s); refusal != "" {
			results[s] = Result{Error: refusal}
		} else if _, queued := results[s]; !queued {
			todo = append(todo, s)
			results[s] = Result{}
		}
	}
	if len(todo) > 0 {
		batch := compileLimited(ctx, todo)
		if batch == nil { // the batch didn't compile: isolate the bad snippet(s)
			batch = map[Snippet]Result{}
			for _, s := range todo {
				for k, v := range compileLimited(ctx, []Snippet{s}) {
					batch[k] = v
				}
			}
		}
		for _, s := range todo {
			r, ok := batch[s]
			if !ok {
				r = Result{Error: "LaTeX error"}
			}
			results[s] = r
			cache.Add(s, r)
		}
	}
	out := make([]Result, len(snippets))
	for i, s := range snippets {
		out[i] = results[s]
	}
	return out
}

func compileLimited(ctx context.Context, snippets []Snippet) map[Snippet]Result {
	semaphore <- struct{}{}
	defer func() { <-semaphore }()
	return compile(ctx, snippets)
}

// Refusal explains why a snippet won't be compiled, or "" when it may be.
func Refusal(s Snippet) string {
	if len([]rune(s.Tex)) > MaxTexChars {
		return fmt.Sprintf("Too long (over %d characters).", MaxTexChars)
	}
	if m, _ := forbiddenRE.FindStringMatch(s.Tex); m != nil {
		return fmt.Sprintf("'%s' is not allowed; use only math / chemfig commands.", m.String())
	}
	return ""
}

var displayEnvs = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`\\(begin|end)\{(?:align|alignat|flalign|eqnarray)\*?\}`), `\${1}{aligned}`},
	{regexp.MustCompile(`\\(begin|end)\{(?:gather|multline)\*?\}`), `\${1}{gathered}`},
	{regexp.MustCompile(`\\(?:begin|end)\{(?:equation|displaymath)\*?\}`), ""},
}

// Equation numbering has no meaning in a snippet
var numberingRE = regexp2.MustCompile(`\\(?:label|tag\*?)\{[^{}]*\}|\\(?:nonumber|notag)(?![A-Za-z])`, regexp2.None)

var (
	arrowRE     = regexp.MustCompile(`\\arrow\s*(?:\([^)]*\))?\s*\{`)
	chemnameRE  = regexp.MustCompile(`\\chemname\*?\s*\{`)
	mathOnlyCmd = `\\(?:[vV]ar)?(?:alpha|beta|[gG]amma|[dD]elta|epsilon|zeta|eta|[tT]heta|iota|kappa|[lL]ambda|mu|nu|[xX]i` +
		`|[pP]i|rho|[sS]igma|tau|[uU]psilon|[pP]hi|chi|[pP]si|[oO]mega|pm|mp|times|cdot|circ|to|infty|approx|sim` +
		`|leq?|geq?|neq|[lL]eftarrow|[rR]ightarrow|[lL]eftrightarrow|rightleftharpoons|uparrow|downarrow)(?![A-Za-z])`
	// A word that needs math mode: has a sub/superscript or a math-only command
	scriptWordRE = regexp2.MustCompile(`[^\s,;]*(?:[_^]|`+mathOnlyCmd+`)[^\s,;]*`, regexp2.None)
	protectedRE  = regexp.MustCompile(`\\ce\s*\{|\$|\\\(|\\ensuremath\s*\{`)
)

// matchingBrace is the index of the `}` closing the `{` at open, or -1.
func matchingBrace(text string, open int) int {
	depth := 0
	for i := open; i < len(text); i++ {
		escaped := i > 0 && text[i-1] == '\\'
		if text[i] == '{' && !escaped {
			depth++
		} else if text[i] == '}' && !escaped {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// mathSafeText turns a text-mode label's formula words (CH_3I, Fe^{3+}, \Delta) into upright math.
//
// chemfig typesets arrow labels and \chemname captions in text mode, where `_`, `^` and math-only commands are
// LaTeX errors. Parts already in math or \ce{...} are left as written.
func mathSafeText(text string) string {
	if strings.Contains(text, "$") || strings.Contains(text, `\(`) { // the label manages math itself
		return text
	}
	var out strings.Builder
	pos := 0
	for _, loc := range protectedRE.FindAllStringIndex(text, -1) {
		if loc[0] < pos {
			continue
		}
		if !strings.HasSuffix(text[loc[0]:loc[1]], "{") {
			continue
		}
		closeAt := matchingBrace(text, loc[1]-1)
		if closeAt < 0 {
			continue
		}
		out.WriteString(wrapFormulaWords(text[pos:loc[0]]))
		out.WriteString(text[loc[0] : closeAt+1])
		pos = closeAt + 1
	}
	out.WriteString(wrapFormulaWords(text[pos:]))
	return out.String()
}

func wrapFormulaWords(text string) string {
	out, err := scriptWordRE.ReplaceFunc(text, func(m regexp2.Match) string {
		return `\ensuremath{\mathrm{` + m.String() + `}}`
	}, -1, -1)
	if err != nil {
		return text
	}
	return out
}

// fixBracketLabels applies mathSafeText to each top-level [...] label inside an \arrow{...} argument.
func fixBracketLabels(arg string) string {
	var out strings.Builder
	depth, i, n := 0, 0, len(arg)
	braceDelta := func(c byte) int {
		switch c {
		case '{':
			return 1
		case '}':
			return -1
		}
		return 0
	}
	for i < n {
		ch := arg[i]
		if ch == '[' && depth == 0 {
			j, inner := i+1, 0
			for j < n && !(arg[j] == ']' && inner == 0) {
				inner += braceDelta(arg[j])
				j++
			}
			if j >= n { // unterminated label: leave the rest as written
				out.WriteString(arg[i:])
				break
			}
			out.WriteString("[" + mathSafeText(arg[i+1:j]) + "]")
			i = j + 1
			continue
		}
		depth += braceDelta(ch)
		out.WriteByte(ch)
		i++
	}
	return out.String()
}

// FixChemfigLabels makes text-mode labels in a chemfig scheme safe for formulas (arrow labels, \chemname captions).
func FixChemfigLabels(code string) string {
	for pass, pattern := range []*regexp.Regexp{arrowRE, chemnameRE} {
		var result strings.Builder
		pos := 0
		for _, loc := range pattern.FindAllStringIndex(code, -1) {
			if loc[0] < pos {
				continue
			}
			open := loc[1] - 1
			closeAt := matchingBrace(code, open)
			if closeAt < 0 {
				break
			}
			if pass == 0 {
				result.WriteString(code[pos:open+1] + fixBracketLabels(code[open+1:closeAt]))
				pos = closeAt
				continue
			}
			// \chemname{<structure>}{<caption>}: fix the second argument
			secondOpen := strings.Index(code[closeAt+1:], "{")
			if secondOpen < 0 {
				continue
			}
			secondOpen += closeAt + 1
			if strings.TrimSpace(code[closeAt+1:secondOpen]) != "" {
				continue
			}
			secondClose := matchingBrace(code, secondOpen)
			if secondClose < 0 {
				break
			}
			result.WriteString(code[pos:secondOpen+1] + mathSafeText(code[secondOpen+1:secondClose]))
			pos = secondClose
		}
		result.WriteString(code[pos:])
		code = result.String()
	}
	return code
}

func body(s Snippet) string {
	switch s.Kind {
	case "chemfig":
		code := strings.TrimSpace(s.Tex)
		// Models sometimes write just the molecule (`*6(=-=-=-)`) without the \chemfig{...} wrapper
		if !strings.HasPrefix(code, `\`) {
			code = `\chemfig{` + code + `}`
		}
		return FixChemfigLabels(code)
	case "inline":
		return "$" + s.Tex + "$"
	}
	tex := strings.TrimSpace(s.Tex)
	for _, env := range displayEnvs { // equation-style environments only work in vertical mode; use inner forms
		tex = env.re.ReplaceAllString(tex, env.repl)
	}
	if replaced, err := numberingRE.Replace(tex, "", -1, -1); err == nil {
		tex = replaced
	}
	if strings.Contains(tex, `\\`) && !strings.Contains(tex, "{aligned}") && !strings.Contains(tex, "{gathered}") {
		if strings.Contains(tex, "&") {
			tex = `\begin{aligned}` + tex + `\end{aligned}`
		} else {
			tex = `\begin{gathered}` + tex + `\end{gathered}`
		}
	}
	return `$\displaystyle ` + tex + "$"
}

func document(snippets []Snippet) string {
	texts := make([]string, len(snippets))
	hasChemfig := false
	for i, s := range snippets {
		texts[i] = s.Tex
		if s.Kind == "chemfig" {
			hasChemfig = true
		}
	}
	joined := strings.Join(texts, " ")
	packages := []string{`\usepackage{amsmath,amssymb}`}
	if strings.Contains(joined, `\ce`) || strings.Contains(joined, `\pu`) || hasChemfig {
		packages = append(packages, `\usepackage[version=4]{mhchem}`)
	}
	if hasChemfig || strings.Contains(joined, `\chemfig`) {
		packages = append([]string{`\def\pgfsysdriver{pgfsys-dvisvgm.def}`}, packages...)
		packages = append(packages, `\usepackage{chemfig}`)
	}
	pages := make([]string, len(snippets))
	for i, s := range snippets {
		pages[i] = fmt.Sprintf(`\snip{%d}{%s}`, i+1, body(s))
	}
	pad, pad2 := jsonval.FormatFloat(PadPt)+"pt", jsonval.FormatFloat(2*PadPt)+"pt"
	return "\\documentclass[10pt]{article}\n" + strings.Join(packages, "\n") + "\n\\pagestyle{empty}\n" +
		"\\hoffset=-1in \\voffset=-1in\n" +
		"\\newsavebox\\snipbox\n" +
		// One page per snippet, exactly the size of its box plus padding; log the box metrics for alignment
		"\\newcommand\\snip[2]{\\sbox\\snipbox{#2}%\n" +
		"\\typeout{::SNIP #1 \\the\\wd\\snipbox,\\the\\ht\\snipbox,\\the\\dp\\snipbox::}%\n" +
		"\\shipout\\hbox{\\special{papersize=\\the\\dimexpr\\wd\\snipbox+" + pad2 + "\\relax," +
		"\\the\\dimexpr\\ht\\snipbox+\\dp\\snipbox+" + pad2 + "\\relax}%\n" +
		"\\vbox{\\kern" + pad + "\\hbox{\\kern" + pad + "\\usebox\\snipbox}\\kern" + pad + "}}}\n" +
		"\\begin{document}\n" + strings.Join(pages, "\n") + "\n\\end{document}\n"
}

// compile builds snippets in one document. nil if LaTeX failed (with one snippet: that snippet's error).
func compile(ctx context.Context, snippets []Snippet) map[Snippet]Result {
	tmp, err := os.MkdirTemp("", "latex-svg-")
	if err != nil {
		return nil
	}
	defer os.RemoveAll(tmp)
	if err := os.WriteFile(filepath.Join(tmp, "doc.tex"), []byte(document(snippets)), 0o644); err != nil {
		return nil
	}
	env := append(os.Environ(), "openin_any=p", "openout_any=p", "shell_escape=f")
	if !run(ctx, tmp, env, "latex", "-no-shell-escape", "-interaction=nonstopmode", "-halt-on-error", "doc.tex") {
		if len(snippets) == 1 {
			return map[Snippet]Result{snippets[0]: {Error: latexError(tmp)}}
		}
		return nil
	}
	// --no-fonts=1: glyphs as inline <path>s (no <use> references, which front-end sanitizers strip)
	if !run(ctx, tmp, env, "dvisvgm", "--no-fonts=1", "--bbox=papersize", "--page=1-", "-o", "page-%p.svg", "doc.dvi") {
		out := map[Snippet]Result{}
		for _, s := range snippets {
			out[s] = Result{Error: "SVG conversion failed"}
		}
		return out
	}
	type box struct{ w, h, d float64 }
	boxes := map[int]box{}
	for _, m := range boxRE.FindAllStringSubmatch(readFile(tmp, "doc.log"), -1) {
		n, _ := strconv.Atoi(m[1])
		w, _ := strconv.ParseFloat(m[2], 64)
		h, _ := strconv.ParseFloat(m[3], 64)
		d, _ := strconv.ParseFloat(m[4], 64)
		boxes[n] = box{w, h, d}
	}
	pages := map[int]string{}
	entries, _ := os.ReadDir(tmp)
	for _, e := range entries {
		if m := pageRE.FindStringSubmatch(e.Name()); m != nil {
			n, _ := strconv.Atoi(m[1])
			pages[n] = readFile(tmp, e.Name())
		}
	}
	if len(snippets) == 1 && len(pages) == 0 {
		if _, err := os.Stat(filepath.Join(tmp, "page.svg")); err == nil {
			pages[1] = readFile(tmp, "page.svg")
		}
	}
	out := map[Snippet]Result{}
	for i, s := range snippets {
		b, ok := boxes[i+1]
		svg := ""
		if ok {
			svg = finishSVG(pages[i+1], b.w, b.h, b.d, s.Kind)
		}
		if svg != "" {
			out[s] = Result{SVG: svg}
		} else {
			out[s] = Result{Error: "SVG conversion failed"}
		}
	}
	return out
}

func finishSVG(svg string, width, height, depth float64, kind string) string {
	if svg == "" {
		return ""
	}
	start := strings.Index(svg, "<svg")
	if start == -1 || activeSVGRE.MatchString(svg) {
		return ""
	}
	svg = svg[start:]
	k := scale[kind] / PtPerEm
	wEm := (width + 2*PadPt) * k
	hEm := (height + depth + 2*PadPt) * k
	valign := -(depth + PadPt) * k
	// Size in em so it scales with the text; inline math sits on the text baseline
	svg = replaceFirst(widthRE, svg, fmt.Sprintf(" width='%.3fem'", wEm))
	svg = replaceFirst(heightRE, svg, fmt.Sprintf(" height='%.3fem'", hEm))
	// Draw in the page's text color: explicit black -> currentColor; implicit (default black) fills inherit it
	svg = blackRE.ReplaceAllStringFunc(svg, func(m string) string {
		attr, value, _ := strings.Cut(m, "=")
		return attr + "=" + value[:1] + "currentColor" + value[:1]
	})
	style := fmt.Sprintf("vertical-align:%.3fem;overflow:visible", valign)
	return strings.Replace(svg, "<svg ", "<svg fill='currentColor' style='"+style+"' ", 1)
}

func replaceFirst(re *regexp.Regexp, s, repl string) string {
	loc := re.FindStringIndex(s)
	if loc == nil {
		return s
	}
	return s[:loc[0]] + repl + s[loc[1]:]
}

func latexError(tmp string) string {
	log := readFile(tmp, "doc.log")
	loc := errorRE.FindStringIndex(log)
	if loc == nil {
		return "LaTeX error"
	}
	if strings.Contains(log[loc[0]:loc[1]], `File ended while scanning use of \snip`) || strings.Contains(log, "Runaway argument") {
		return "unbalanced braces: a '{' is never closed"
	}
	// The error line plus the context line(s) TeX prints after it ("l.12 ...")
	lines := strings.Split(log[loc[0]:], "\n")
	if len(lines) > 4 {
		lines = lines[:4]
	}
	var kept []string
	for _, line := range lines {
		if t := strings.TrimSpace(line); t != "" {
			kept = append(kept, t)
		}
	}
	out := strings.Join(kept, " ")
	if r := []rune(out); len(r) > 500 {
		out = string(r[:500])
	}
	return out
}

func readFile(dir, name string) string {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.ToValidUTF8(string(data), "\uFFFD")
}

func run(ctx context.Context, dir string, env []string, name string, args ...string) bool {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	return cmd.Run() == nil
}
