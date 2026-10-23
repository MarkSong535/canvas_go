package agent

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"

	"canvas_go/internal/jsonval"
	"canvas_go/internal/tools"
)

//go:embed prompts/general_agent.yaml
var defaultPromptYAML []byte

// PromptTemplates is the parsed prompt YAML: top-level strings and nested sections (planning, final_answer, ...).
type PromptTemplates map[string]any

// LoadPromptTemplates reads a prompt YAML file, or the built-in general agent prompts when path is "".
func LoadPromptTemplates(path string) (PromptTemplates, error) {
	data := defaultPromptYAML
	if path != "" {
		if !filepath.IsAbs(path) {
			path = filepath.Join(ProjectRoot(), path)
		}
		var err error
		if data, err = os.ReadFile(path); err != nil {
			return nil, err
		}
	}
	var out PromptTemplates
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("bad prompt templates: %w", err)
	}
	return out, nil
}

// Get returns a template by path, e.g. Get("final_answer", "pre_messages"); "" when missing.
func (p PromptTemplates) Get(keys ...string) (string, bool) {
	var cur any = map[string]any(p)
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		if cur, ok = m[k]; !ok {
			return "", false
		}
	}
	s, ok := cur.(string)
	return s, ok
}

// ProjectRoot is where relative paths (prompt files, workdir) resolve: CANVAS_GO_ROOT, else the working directory.
func ProjectRoot() string {
	if root := os.Getenv("CANVAS_GO_ROOT"); root != "" {
		return root
	}
	wd, _ := os.Getwd()
	return wd
}

// PopulateTemplate renders a template; a missing variable is an error.
func PopulateTemplate(text string, variables map[string]any) (string, error) {
	tmpl, err := template.New("prompt").Option("missingkey=error").Parse(text)
	if err != nil {
		return "", fmt.Errorf("prompt template error: %v", err)
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, variables); err != nil {
		return "", fmt.Errorf("prompt template error: %v", err)
	}
	return b.String(), nil
}

// toolView is what templates see of a tool.
type toolView struct {
	Name        string
	Description string
	Properties  string // the inputs as a JSON schema object
	OutputType  string
}

func toolViews(ts []tools.Tool) []toolView {
	out := make([]toolView, len(ts))
	for i, t := range ts {
		out[i] = toolView{Name: t.Name(), Description: t.Description(),
			Properties: jsonval.JSON(tools.PropertiesJSON(t.Parameters(), false)), OutputType: t.OutputType()}
	}
	return out
}
