// Package memory holds the agent's memory: the steps of each task and how they replay as chat messages.
package memory

import (
	"encoding/json"
	"fmt"
	"strings"

	"canvas_go/internal/agenterr"
	"canvas_go/internal/jsonval"
	"canvas_go/internal/logger"
	"canvas_go/internal/models"
)

// ToolCall is a tool call the model made in a step.
type ToolCall struct {
	Name      string
	Arguments any
	ID        string
}

func (tc ToolCall) AsMap() map[string]any {
	return map[string]any{
		"id":   tc.ID,
		"type": "function",
		"function": map[string]any{
			"name":      tc.Name,
			"arguments": MakeJSONSerializable(tc.Arguments),
		},
	}
}

// MemoryStep is one entry in the agent's memory.
type MemoryStep interface {
	ToMessages(summaryMode bool) []*models.ChatMessage
}

// ActionStep is one think/act/observe cycle.
type ActionStep struct {
	StepNumber         int
	Timing             logger.Timing
	ModelInputMessages []*models.ChatMessage
	ToolCalls          []ToolCall
	Error              error
	ModelOutputMessage *models.ChatMessage
	ModelOutput        *string
	Observations       *string
	ActionOutput       any
	TokenUsage         *logger.TokenUsage
	IsFinalAnswer      bool
	ToolResults        map[string]string // tool call id -> that call's observation
}

// AsMap is the step's JSON form.
func (s *ActionStep) AsMap() map[string]any {
	calls := []any{}
	for _, tc := range s.ToolCalls {
		calls = append(calls, tc.AsMap())
	}
	var errMap any
	if e, ok := agenterr.As(s.Error); ok {
		errMap = e.AsMap()
	} else if s.Error != nil {
		errMap = map[string]string{"type": "Error", "message": s.Error.Error()}
	}
	return map[string]any{
		"step_number":          s.StepNumber,
		"timing":               s.Timing,
		"model_input_messages": s.ModelInputMessages,
		"tool_calls":           calls,
		"error":                errMap,
		"model_output_message": s.ModelOutputMessage,
		"model_output":         s.ModelOutput,
		"observations":         s.Observations,
		"action_output":        MakeJSONSerializable(s.ActionOutput),
		"token_usage":          s.TokenUsage,
		"is_final_answer":      s.IsFinalAnswer,
		"tool_results":         s.ToolResults,
	}
}

func retryText(err error) string {
	return fmt.Sprintf("Error:\n%s\nNow let's retry: take care not to repeat previous errors! "+
		"If you have retried several times, try a completely different approach.", err)
}

func (s *ActionStep) ToMessages(summaryMode bool) []*models.ChatMessage {
	allIDs := len(s.ToolCalls) > 0
	for _, tc := range s.ToolCalls {
		if tc.ID == "" {
			allIDs = false
		}
	}
	if allIDs {
		return s.toNativeToolMessages()
	}
	var messages []*models.ChatMessage
	if s.ModelOutput != nil && !summaryMode {
		messages = append(messages, &models.ChatMessage{Role: models.RoleAssistant, Content: models.Text(strings.TrimSpace(*s.ModelOutput))})
	}
	if s.ToolCalls != nil {
		calls := make([]any, len(s.ToolCalls))
		for i, tc := range s.ToolCalls {
			calls[i] = tc.AsMap()
		}
		messages = append(messages, &models.ChatMessage{Role: models.RoleToolCall,
			Content: models.Text("Calling tools:\n" + jsonval.JSON(calls))})
	}
	if s.Observations != nil {
		messages = append(messages, &models.ChatMessage{Role: models.RoleToolResponse,
			Content: models.Text("Observation:\n" + *s.Observations)})
	}
	if s.Error != nil {
		text := "Error:\n" + s.Error.Error() + "\nNow let's retry: take care not to repeat previous errors! If you have retried several times, try a completely different approach.\n"
		if len(s.ToolCalls) > 0 {
			text = "Call id: " + s.ToolCalls[0].ID + "\n" + text
		}
		messages = append(messages, &models.ChatMessage{Role: models.RoleToolResponse, Content: models.Text(text)})
	}
	return messages
}

// toNativeToolMessages replays the step as the model saw it: its tool calls, then one result per call.
// Sending results as native `tool` messages (instead of narrated text) lets the model see that each call already
// completed; without that it tends to repeat the same call until max_steps.
func (s *ActionStep) toNativeToolMessages() []*models.ChatMessage {
	calls := make([]models.ChatMessageToolCall, len(s.ToolCalls))
	for i, tc := range s.ToolCalls {
		calls[i] = models.ChatMessageToolCall{
			Function: models.ChatMessageToolCallFunction{Name: tc.Name, Arguments: MakeJSONSerializable(tc.Arguments)},
			ID:       tc.ID,
			Type:     "function",
		}
	}
	messages := []*models.ChatMessage{{Role: models.RoleAssistant, Content: nil, ToolCalls: calls}}
	for _, tc := range s.ToolCalls {
		var text string
		if result, ok := s.ToolResults[tc.ID]; ok {
			text = result
		} else if s.Error != nil {
			text = retryText(s.Error)
		} else if tc.Name == "final_answer_tool" {
			text = "Final answer delivered to the user."
		} else {
			text = "No result was recorded for this call."
		}
		messages = append(messages, &models.ChatMessage{Role: models.RoleTool, Content: models.Text(text), ToolCallID: tc.ID})
	}
	return messages
}

// PlanningStep is a (re)planning step.
type PlanningStep struct {
	ModelInputMessages []*models.ChatMessage
	ModelOutputMessage *models.ChatMessage
	Plan               string
	Timing             logger.Timing
	TokenUsage         *logger.TokenUsage
}

func (s *PlanningStep) ToMessages(summaryMode bool) []*models.ChatMessage {
	if summaryMode {
		return nil
	}
	return []*models.ChatMessage{
		{Role: models.RoleAssistant, Content: models.Text(strings.TrimSpace(s.Plan))},
		// This second message creates a role change to prevent models from simply continuing the plan message
		{Role: models.RoleUser, Content: models.Text("Now proceed and carry out this plan.")},
	}
}

// TaskStep starts a task (one user question).
type TaskStep struct {
	Task string
}

func (s *TaskStep) ToMessages(summaryMode bool) []*models.ChatMessage {
	return []*models.ChatMessage{{Role: models.RoleUser, Content: models.Text("New task:\n" + s.Task)}}
}

type SystemPromptStep struct{ SystemPrompt string }

func (s *SystemPromptStep) ToMessages(summaryMode bool) []*models.ChatMessage {
	if summaryMode {
		return nil
	}
	return []*models.ChatMessage{{Role: models.RoleSystem, Content: models.Text(s.SystemPrompt)}}
}

type UserPromptStep struct{ UserPrompt string }

func (s *UserPromptStep) ToMessages(summaryMode bool) []*models.ChatMessage {
	if summaryMode {
		return nil
	}
	return []*models.ChatMessage{{Role: models.RoleUser, Content: models.Text(s.UserPrompt)}}
}

// FinalAnswerStep carries the run's final output (it is streamed, never stored in memory).
type FinalAnswerStep struct{ Output any }

func (s *FinalAnswerStep) ToMessages(summaryMode bool) []*models.ChatMessage { return nil }

// AgentMemory is the system prompt, an optional trailing user prompt, and the steps.
type AgentMemory struct {
	SystemPrompt *SystemPromptStep
	UserPrompt   *UserPromptStep
	Steps        []MemoryStep
}

func NewAgentMemory(systemPrompt string, userPrompt *string) *AgentMemory {
	m := &AgentMemory{SystemPrompt: &SystemPromptStep{SystemPrompt: systemPrompt}}
	if userPrompt != nil {
		m.UserPrompt = &UserPromptStep{UserPrompt: *userPrompt}
	}
	return m
}

func (m *AgentMemory) Reset() { m.Steps = nil }

// KeepLastTasks drops the oldest turns so that at most n tasks (and the steps that followed each) remain.
func (m *AgentMemory) KeepLastTasks(n int) {
	var taskIndices []int
	for i, step := range m.Steps {
		if _, ok := step.(*TaskStep); ok {
			taskIndices = append(taskIndices, i)
		}
	}
	if n <= 0 {
		m.Steps = nil
	} else if len(taskIndices) > n {
		m.Steps = append([]MemoryStep(nil), m.Steps[taskIndices[len(taskIndices)-n]:]...)
	}
}

// GetFullSteps is every step's JSON form.
func (m *AgentMemory) GetFullSteps() []any {
	out := make([]any, 0, len(m.Steps))
	for _, step := range m.Steps {
		if a, ok := step.(*ActionStep); ok {
			out = append(out, a.AsMap())
		} else {
			out = append(out, step)
		}
	}
	return out
}

// Replay prints the agent's steps.
func (m *AgentMemory) Replay(l *logger.AgentLogger, detailed bool) {
	l.LogText(logger.Error, "Replaying the agent's steps:")
	l.LogMarkdown(logger.Error, "System prompt", m.SystemPrompt.SystemPrompt)
	if m.UserPrompt != nil {
		l.LogMarkdown(logger.Error, "User prompt", m.UserPrompt.UserPrompt)
	}
	for _, step := range m.Steps {
		switch s := step.(type) {
		case *TaskStep:
			l.LogTask(logger.Error, s.Task, "", "")
		case *ActionStep:
			l.LogRule(logger.Error, fmt.Sprintf("Step %d", s.StepNumber))
			if detailed && s.ModelInputMessages != nil {
				l.LogMessages(logger.Error, messagesAsAny(s.ModelInputMessages))
			}
			if s.ModelOutput != nil {
				l.LogMarkdown(logger.Error, "Agent output:", *s.ModelOutput)
			}
		case *PlanningStep:
			l.LogRule(logger.Error, "Planning step")
			if detailed && s.ModelInputMessages != nil {
				l.LogMessages(logger.Error, messagesAsAny(s.ModelInputMessages))
			}
			l.LogMarkdown(logger.Error, "Agent output:", s.Plan)
		}
	}
}

func messagesAsAny(messages []*models.ChatMessage) []any {
	out := make([]any, len(messages))
	for i, m := range messages {
		out[i] = m
	}
	return out
}

// MakeJSONSerializable makes a value JSON-safe; strings that look like a JSON object/array are parsed.
func MakeJSONSerializable(obj any) any {
	switch x := obj.(type) {
	case nil:
		return nil
	case string:
		if (strings.HasPrefix(x, "{") && strings.HasSuffix(x, "}")) || (strings.HasPrefix(x, "[") && strings.HasSuffix(x, "]")) {
			if parsed, err := jsonval.Decode([]byte(x)); err == nil {
				return MakeJSONSerializable(parsed)
			}
		}
		return x
	case bool, json.Number, int, int64, float64:
		return x
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = MakeJSONSerializable(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[k] = MakeJSONSerializable(v)
		}
		return out
	case fmt.Stringer:
		return x.String()
	}
	return jsonval.Text(obj)
}
