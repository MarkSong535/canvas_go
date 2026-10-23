// Package tools defines the Tool interface the agent calls, the ToolResult it returns, argument binding
// (the model passes named arguments as a JSON object), and the final_answer tool.
package tools

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"canvas_go/internal/jsonval"
)

// AuthorizedTypes are the JSON-schema types a tool input or output may declare.
var AuthorizedTypes = []string{"string", "boolean", "integer", "number", "image", "audio", "array", "object", "any", "null"}

// Property is one tool input.
type Property struct {
	Name        string
	Type        string
	Description string
	Nullable    bool
}

// Schema describes a tool's inputs (properties keep their declared order).
type Schema struct {
	Properties []Property
	Required   []string
}

// Tool is something the agent can call.
type Tool interface {
	Name() string
	Description() string
	Parameters() Schema
	OutputType() string
	// Forward runs the tool with already-checked arguments.
	Forward(ctx context.Context, args Args) (any, error)
}

// Spec carries a tool's static description; embed it to implement the metadata half of Tool.
type Spec struct {
	ToolName        string
	ToolDescription string
	Params          Schema
	Output          string
}

func (s *Spec) Name() string        { return s.ToolName }
func (s *Spec) Description() string { return s.ToolDescription }
func (s *Spec) Parameters() Schema  { return s.Params }
func (s *Spec) OutputType() string {
	if s.Output == "" {
		return "any"
	}
	return s.Output
}

// ToolResult is what tools return: an output, or an error message for the model.
type ToolResult struct {
	Output any
	Error  string
}

// String is what the model sees: "Error: ..." or the output.
func (r ToolResult) String() string {
	if r.Error != "" {
		return "Error: " + r.Error
	}
	return jsonval.Text(r.Output)
}

func OK(output any) ToolResult                 { return ToolResult{Output: output} }
func Fail(message string) ToolResult           { return ToolResult{Error: message} }
func Failf(format string, a ...any) ToolResult { return ToolResult{Error: fmt.Sprintf(format, a...)} }

// PropertiesJSON is the "properties" object of the JSON schema, in declared order.
func PropertiesJSON(s Schema, anyAsString bool) *jsonval.OrderedMap {
	props := jsonval.NewOrderedMap()
	for _, p := range s.Properties {
		prop := jsonval.NewOrderedMap()
		typ := p.Type
		if anyAsString && typ == "any" {
			typ = "string"
		}
		prop.Set("type", typ)
		prop.Set("description", p.Description)
		if p.Nullable {
			prop.Set("nullable", true)
		}
		props.Set(p.Name, prop)
	}
	return props
}

// ParametersJSON is the full parameters schema, as sent in error messages to the model.
func ParametersJSON(s Schema) *jsonval.OrderedMap {
	out := jsonval.NewOrderedMap()
	out.Set("type", "object")
	out.Set("properties", PropertiesJSON(s, false))
	required := make([]any, len(s.Required))
	for i, r := range s.Required {
		required[i] = r
	}
	out.Set("required", required)
	out.Set("additionalProperties", false)
	return out
}

// Validate checks a tool's declaration.
func Validate(t Tool) error {
	if !IsValidName(t.Name()) {
		return fmt.Errorf("invalid tool name %q: use 1-64 letters, digits, '_' or '-'", t.Name())
	}
	for _, p := range t.Parameters().Properties {
		if p.Type == "" || p.Description == "" {
			return fmt.Errorf("Input '%s' should have keys 'type' and 'description'.", p.Name)
		}
		if !authorized(p.Type) {
			return fmt.Errorf("Input '%s': type '%s' is not an authorized value, should be one of %v.", p.Name, p.Type, AuthorizedTypes)
		}
	}
	if !authorized(t.OutputType()) {
		return fmt.Errorf("tool '%s': output type '%s' is not authorized", t.Name(), t.OutputType())
	}
	return nil
}

func authorized(typ string) bool {
	for _, a := range AuthorizedTypes {
		if a == typ {
			return true
		}
	}
	return false
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// IsValidName reports whether a tool or agent name is a valid function name for the chat-completions API.
func IsValidName(name string) bool { return nameRE.MatchString(name) }

// ArgumentError is a call whose arguments don't match the tool's inputs.
type ArgumentError struct{ Message string }

func (e *ArgumentError) Error() string { return e.Message }

// Args are a call's named arguments.
type Args map[string]any

// Call checks `arguments` (an object of named arguments, or a bare value for the first input) against the
// tool's inputs and runs it.
func Call(ctx context.Context, t Tool, arguments any) (any, error) {
	args, err := BindArgs(t, arguments)
	if err != nil {
		return nil, err
	}
	return t.Forward(ctx, args)
}

// BindArgs turns call arguments into named arguments, rejecting unknown and missing ones.
func BindArgs(t Tool, arguments any) (Args, error) {
	schema := t.Parameters()
	known := map[string]bool{}
	for _, p := range schema.Properties {
		known[p.Name] = true
	}
	args := Args{}
	switch a := arguments.(type) {
	case nil:
	case map[string]any:
		for k, v := range a {
			if !known[k] {
				return nil, &ArgumentError{fmt.Sprintf("%s: unexpected argument %q", t.Name(), k)}
			}
			args[k] = v
		}
	case Args:
		for k, v := range a {
			if !known[k] {
				return nil, &ArgumentError{fmt.Sprintf("%s: unexpected argument %q", t.Name(), k)}
			}
			args[k] = v
		}
	default:
		// A bare value goes to the first input
		if len(schema.Properties) == 0 {
			return nil, &ArgumentError{fmt.Sprintf("%s: takes no arguments", t.Name())}
		}
		args[schema.Properties[0].Name] = a
	}
	var missing []string
	for _, p := range schema.Properties {
		if _, ok := args[p.Name]; !ok && !p.Nullable {
			missing = append(missing, strconv.Quote(p.Name))
		}
	}
	if len(missing) > 0 {
		return nil, &ArgumentError{fmt.Sprintf("%s: missing required argument(s) %s", t.Name(), strings.Join(missing, ", "))}
	}
	return args, nil
}

// Str is a string argument (numbers and booleans become their text); missing or null -> def.
func (a Args) Str(name, def string) string {
	v, ok := a[name]
	if !ok || v == nil {
		return def
	}
	return jsonval.Text(v)
}

// Int is an integer argument; missing, null or unparsable -> def.
func (a Args) Int(name string, def int) int {
	v, ok := a[name]
	if !ok || v == nil {
		return def
	}
	if f, ok := jsonval.Float(v); ok {
		return int(f)
	}
	if s, ok := v.(string); ok {
		if s == "" {
			return def
		}
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			return n
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return int(f)
		}
	}
	return def
}

// Bool is a boolean argument (non-empty, non-zero values count as true); missing or null -> def.
func (a Args) Bool(name string, def bool) bool {
	v, ok := a[name]
	if !ok || v == nil {
		return def
	}
	return jsonval.Truthy(v)
}

// FinalAnswerTool returns the agent's final answer.
type FinalAnswerTool struct{ Spec }

func NewFinalAnswerTool() *FinalAnswerTool {
	return &FinalAnswerTool{Spec{
		ToolName:        "final_answer_tool",
		ToolDescription: "Provides a final answer to the given problem.",
		Params: Schema{
			Properties: []Property{{Name: "answer", Type: "string", Description: "The final answer to the problem."}},
			Required:   []string{"answer"},
		},
	}}
}

func (t *FinalAnswerTool) Forward(ctx context.Context, args Args) (any, error) {
	return ToolResult{Output: args["answer"]}, nil
}
