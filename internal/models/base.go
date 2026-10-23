// Package models holds chat messages, the Model interface, the OpenAI-compatible model and the model manager.
package models

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/titanous/json5"

	"canvas_go/internal/jsonval"
	"canvas_go/internal/logger"
	"canvas_go/internal/tools"
)

type MessageRole string

const (
	RoleUser         MessageRole = "user"
	RoleAssistant    MessageRole = "assistant"
	RoleSystem       MessageRole = "system"
	RoleToolCall     MessageRole = "tool-call"
	RoleToolResponse MessageRole = "tool-response"
	RoleTool         MessageRole = "tool" // Native tool-result message (OpenAI `role: "tool"` with `tool_call_id`)
)

// Roles lists every valid role.
func Roles() []MessageRole {
	return []MessageRole{RoleUser, RoleAssistant, RoleSystem, RoleToolCall, RoleToolResponse, RoleTool}
}

// ToolRoleConversions maps the narrated tool roles to API roles.
var ToolRoleConversions = map[MessageRole]MessageRole{
	RoleToolCall:     RoleAssistant,
	RoleToolResponse: RoleUser,
}

// ContentPart is one element of a message's content list, e.g. {"type": "text", "text": "..."}.
type ContentPart = map[string]any

// Text builds a one-part text content list.
func Text(text string) []ContentPart { return []ContentPart{{"type": "text", "text": text}} }

type ChatMessageToolCallFunction struct {
	Arguments   any    `json:"arguments"` // a JSON string, or the parsed value
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type ChatMessageToolCall struct {
	Function ChatMessageToolCallFunction `json:"function"`
	ID       string                      `json:"id"`
	Type     string                      `json:"type"`
}

func (c *ChatMessageToolCall) String() string {
	return fmt.Sprintf("Call: %s: Calling %s with arguments: %s", c.ID, c.Function.Name, jsonval.Text(c.Function.Arguments))
}

// ChatMessage is one message; Content is nil, a string, or []ContentPart.
type ChatMessage struct {
	Role       MessageRole           `json:"role"`
	Content    any                   `json:"content"`
	ToolCalls  []ChatMessageToolCall `json:"tool_calls,omitempty"`
	Raw        any                   `json:"-"`
	TokenUsage *logger.TokenUsage    `json:"token_usage,omitempty"`
	ToolCallID string                `json:"tool_call_id,omitempty"`
}

// ContentText is the message's text: a string content, or its text parts joined.
func (m *ChatMessage) ContentText() string {
	switch c := m.Content.(type) {
	case nil:
		return ""
	case string:
		return c
	case []ContentPart:
		var parts []string
		for _, p := range c {
			if p["type"] == "text" {
				parts = append(parts, jsonval.Text(p["text"]))
			}
		}
		return strings.Join(parts, "\n")
	}
	return jsonval.Text(m.Content)
}

// RenderAsMarkdown is the content plus a JSON line per tool call.
func (m *ChatMessage) RenderAsMarkdown() string {
	rendered := jsonval.Text(m.Content)
	var calls []string
	for _, tc := range m.ToolCalls {
		calls = append(calls, jsonval.JSON(map[string]any{"tool": tc.Function.Name, "arguments": tc.Function.Arguments}))
	}
	return rendered + strings.Join(calls, "\n")
}

// ChatMessageToolCallStreamDelta is a streamed piece of a tool call.
type ChatMessageToolCallStreamDelta struct {
	Index    *int
	ID       string
	Type     string
	Function *ChatMessageToolCallFunction // Arguments is a string fragment here
}

// ChatMessageStreamDelta is one streamed piece of a model reply.
type ChatMessageStreamDelta struct {
	Content    string
	ToolCalls  []ChatMessageToolCallStreamDelta
	TokenUsage *logger.TokenUsage
}

// AgglomerateStreamDeltas folds stream deltas into one message.
func AgglomerateStreamDeltas(deltas []*ChatMessageStreamDelta, role MessageRole) (*ChatMessage, error) {
	type acc struct {
		id, typ, name string
		args          strings.Builder
	}
	accumulated := map[int]*acc{}
	var order []int
	var content strings.Builder
	inputTokens, outputTokens := 0, 0
	for _, d := range deltas {
		if d.TokenUsage != nil {
			inputTokens += d.TokenUsage.InputTokens
			outputTokens += d.TokenUsage.OutputTokens
		}
		content.WriteString(d.Content)
		for _, tc := range d.ToolCalls {
			if tc.Index == nil {
				return nil, fmt.Errorf("Any call index is not provided in tool delta: %+v", tc)
			}
			call, ok := accumulated[*tc.Index]
			if !ok {
				call = &acc{id: tc.ID, typ: tc.Type}
				accumulated[*tc.Index] = call
				order = append(order, *tc.Index)
			}
			if tc.ID != "" {
				call.id = tc.ID
			}
			if tc.Type != "" {
				call.typ = tc.Type
			}
			if tc.Function != nil {
				if tc.Function.Name != "" {
					call.name = tc.Function.Name
				}
				if s, ok := tc.Function.Arguments.(string); ok && s != "" {
					call.args.WriteString(s)
				}
			}
		}
	}
	msg := &ChatMessage{Role: role, Content: content.String(), ToolCalls: []ChatMessageToolCall{},
		TokenUsage: logger.NewTokenUsage(inputTokens, outputTokens)}
	for _, index := range order {
		call := accumulated[index]
		msg.ToolCalls = append(msg.ToolCalls, ChatMessageToolCall{
			Function: ChatMessageToolCallFunction{Name: call.name, Arguments: call.args.String()},
			ID:       call.id,
			Type:     "function",
		})
	}
	return msg, nil
}

// parseJSON5 parses lenient JSON (comments, single quotes, unquoted keys, trailing commas); numbers are float64.
func parseJSON5(text string) (any, error) {
	var v any
	err := json5.Unmarshal([]byte(text), &v)
	return v, err
}

// escapeControlsInStrings escapes raw control characters that appear inside quoted strings.
func escapeControlsInStrings(text string) string {
	var b strings.Builder
	var quote rune
	escaped := false
	for _, r := range text {
		switch {
		case quote == 0:
			if r == '"' || r == '\'' {
				quote = r
			}
		case escaped:
			escaped = false
		case r == '\\':
			escaped = true
		case r == quote:
			quote = 0
		case r < 0x20:
			fmt.Fprintf(&b, "\\u%04x", r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ParseJSONIfNeeded parses a JSON string (strict JSON first, then JSON5); anything else is returned as is.
func ParseJSONIfNeeded(arguments any) any {
	s, ok := arguments.(string)
	if !ok {
		return arguments
	}
	if v, err := jsonval.Decode([]byte(s)); err == nil {
		return v
	}
	if v, err := parseJSON5(s); err == nil {
		return v
	}
	return s
}

var stopUnsupportedRE = regexp.MustCompile(`^(o\d.*|gpt-5.*|gpt-6.*)$`)

// SupportsStopParameter is false for reasoning models: the o-series and every gpt-5.x / gpt-6.x variant.
func SupportsStopParameter(modelID string) bool {
	parts := strings.Split(modelID, "/")
	return !stopUnsupportedRE.MatchString(parts[len(parts)-1])
}

// GenerateOptions are the optional arguments of a model call.
type GenerateOptions struct {
	StopSequences   []string
	ResponseFormat  map[string]any
	ToolsToCallFrom []tools.Tool
	// Extra overrides request parameters (e.g. "reasoning_effort").
	Extra map[string]any
}

// Model generates chat replies.
type Model interface {
	ModelID() string
	Generate(ctx context.Context, messages []*ChatMessage, opts GenerateOptions) (*ChatMessage, error)
}

// StreamingModel can also stream its reply as deltas.
type StreamingModel interface {
	Model
	GenerateStream(ctx context.Context, messages []*ChatMessage, opts GenerateOptions, yield func(*ChatMessageStreamDelta) error) error
}

// ToolCallParser turns a text reply into tool calls (for replies without native tool calls).
type ToolCallParser interface {
	ParseToolCalls(message *ChatMessage) (*ChatMessage, error)
}

// ParseToolCalls is the default text -> tool call parsing.
func ParseToolCalls(message *ChatMessage, toolNameKey, toolArgumentsKey string) (*ChatMessage, error) {
	message.Role = RoleAssistant
	if len(message.ToolCalls) == 0 {
		if message.Content == nil {
			return nil, fmt.Errorf("Message contains no content and no tool calls")
		}
		call, err := GetToolCallFromText(message.ContentText(), toolNameKey, toolArgumentsKey)
		if err != nil {
			return nil, err
		}
		message.ToolCalls = []ChatMessageToolCall{*call}
	}
	for i := range message.ToolCalls {
		message.ToolCalls[i].Function.Arguments = ParseJSONIfNeeded(message.ToolCalls[i].Function.Arguments)
	}
	return message, nil
}

// GetToolCallFromText reads a {"name": ..., "arguments": ...} blob out of model text.
func GetToolCallFromText(text, toolNameKey, toolArgumentsKey string) (*ChatMessageToolCall, error) {
	blob, _, err := ParseJSONBlob(text)
	if err != nil {
		return nil, err
	}
	name, ok := blob[toolNameKey]
	if !ok {
		keys := make([]string, 0, len(blob))
		for k := range blob {
			keys = append(keys, k)
		}
		return nil, fmt.Errorf("key %q not found in the generated tool call; got keys %v instead", toolNameKey, keys)
	}
	arguments := blob[toolArgumentsKey]
	if s, ok := arguments.(string); ok {
		arguments = ParseJSONIfNeeded(s)
	}
	return &ChatMessageToolCall{
		ID:       newUUID(),
		Type:     "function",
		Function: ChatMessageToolCallFunction{Name: jsonval.Text(name), Arguments: arguments},
	}, nil
}

// ParseJSONBlob extracts the JSON blob in model output; returns its "function" object and the text before it.
func ParseJSONBlob(blob string) (map[string]any, string, error) {
	if strings.Contains(blob, "Calling tools:") {
		parts := strings.Split(blob, "Calling tools:")
		blob = parts[len(parts)-1]
	}
	first := strings.Index(blob, "{")
	last := strings.LastIndex(blob, "}")
	if first < 0 || last < 0 || last < first {
		return nil, "", fmt.Errorf("The model output does not contain any JSON blob.")
	}
	// Models sometimes put raw newlines or tabs inside strings; escape them so the blob still parses
	data, err := parseJSON5(escapeControlsInStrings(blob[first : last+1]))
	if err != nil {
		return nil, "", fmt.Errorf("The JSON blob you used is invalid due to the following error: %v.\nJSON blob was: %s", err, blob)
	}
	obj, ok := data.(map[string]any)
	if !ok {
		return nil, "", fmt.Errorf("The JSON blob you used is invalid: expected an object")
	}
	function, ok := obj["function"].(map[string]any)
	if !ok {
		return nil, "", fmt.Errorf("'function'")
	}
	return function, blob[:first], nil
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// ToolJSONSchema is the function-calling schema for a tool ("any" inputs are sent as strings).
func ToolJSONSchema(t tools.Tool) *jsonval.OrderedMap {
	schema := t.Parameters()
	required := []any{}
	for _, p := range schema.Properties {
		if !p.Nullable {
			required = append(required, p.Name)
		}
	}
	params := jsonval.NewOrderedMap()
	params.Set("type", "object")
	params.Set("properties", tools.PropertiesJSON(schema, true))
	params.Set("required", required)
	function := jsonval.NewOrderedMap()
	function.Set("name", t.Name())
	function.Set("description", t.Description())
	function.Set("parameters", params)
	out := jsonval.NewOrderedMap()
	out.Set("type", "function")
	out.Set("function", function)
	return out
}

// CleanMessageList converts messages to chat-completions message objects. Consecutive messages with the same role are
// merged; native tool-call messages (assistant tool_calls, then one `tool` message per call) never are.
func CleanMessageList(messages []*ChatMessage, roleConversions map[MessageRole]MessageRole, flattenMessagesAsText bool) ([]map[string]any, error) {
	var out []map[string]any
	valid := map[MessageRole]bool{}
	for _, r := range Roles() {
		valid[r] = true
	}
	for _, message := range messages {
		if !valid[message.Role] {
			return nil, fmt.Errorf("Incorrect role %s, only %v are supported for now.", message.Role, Roles())
		}
		role := message.Role
		if converted, ok := roleConversions[role]; ok {
			role = converted
		}
		content := copyContent(message.Content)

		if len(message.ToolCalls) > 0 || role == RoleTool {
			entryContent := content
			if parts, ok := content.([]ContentPart); ok && flattenMessagesAsText {
				var texts []string
				for _, el := range parts {
					if el["type"] == "text" {
						texts = append(texts, jsonval.Text(el["text"]))
					}
				}
				entryContent = strings.Join(texts, "\n")
			}
			entry := map[string]any{"role": string(role), "content": entryContent}
			if len(message.ToolCalls) > 0 {
				calls := make([]any, len(message.ToolCalls))
				for i, tc := range message.ToolCalls {
					args, ok := tc.Function.Arguments.(string)
					if !ok {
						args = jsonval.JSON(tc.Function.Arguments)
					}
					calls[i] = map[string]any{
						"id":       tc.ID,
						"type":     "function",
						"function": map[string]any{"name": tc.Function.Name, "arguments": args},
					}
				}
				entry["tool_calls"] = calls
			}
			if message.ToolCallID != "" {
				entry["tool_call_id"] = message.ToolCallID
			}
			out = append(out, entry)
			continue
		}

		var last map[string]any
		if len(out) > 0 {
			last = out[len(out)-1]
		}
		_, lastHasCalls := last["tool_calls"]
		if last != nil && string(role) == last["role"] && !lastHasCalls && last["role"] != string(RoleTool) {
			parts, ok := content.([]ContentPart)
			if !ok {
				return nil, fmt.Errorf("Error: wrong content:%s", jsonval.Text(content))
			}
			if flattenMessagesAsText {
				last["content"] = jsonval.Text(last["content"]) + "\n" + jsonval.Text(parts[0]["text"])
			} else {
				lastParts := last["content"].([]ContentPart)
				for _, el := range parts {
					if el["type"] == "text" && len(lastParts) > 0 && lastParts[len(lastParts)-1]["type"] == "text" {
						lastParts[len(lastParts)-1]["text"] = jsonval.Text(lastParts[len(lastParts)-1]["text"]) + "\n" + jsonval.Text(el["text"])
					} else {
						lastParts = append(lastParts, el)
					}
				}
				last["content"] = lastParts
			}
		} else {
			entryContent := content
			if parts, ok := content.([]ContentPart); ok && flattenMessagesAsText && len(parts) > 0 {
				entryContent = parts[0]["text"]
			}
			out = append(out, map[string]any{"role": string(role), "content": entryContent})
		}
	}
	return out, nil
}

func copyContent(content any) any {
	parts, ok := content.([]ContentPart)
	if !ok {
		return content
	}
	out := make([]ContentPart, len(parts))
	for i, p := range parts {
		c := ContentPart{}
		for k, v := range p {
			c[k] = v
		}
		out[i] = c
	}
	return out
}

// marshalForLog is used by loggers that dump messages.
func marshalForLog(v any) string {
	out, _ := json.Marshal(v)
	return string(out)
}
