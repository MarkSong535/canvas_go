// Package tests holds the end-to-end tests. The OpenAI client is replaced by a scripted simulated that streams
// chunks like the real API, so no API key, Canvas account or network is needed. Run with `go test ./...`.
package tests

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/openai/openai-go/v3"

	"canvas_go/internal/agent"
	"canvas_go/internal/logger"
	"canvas_go/internal/models"
	"canvas_go/internal/tools"
)

func init() {
	// Placeholders so the config's Canvas tools can be built; the simulated model never lets them reach the network
	setDefault("OPENAI_API_KEY", "sk-test")
	setDefault("CANVAS_ACCESS_TOKEN", "test-token")
	setDefault("CANVAS_URL", "https://canvas.example.com")
	logger.Default.SetOutput(io.Discard)
}

func setDefault(key, value string) {
	if _, ok := os.LookupEnv(key); !ok {
		os.Setenv(key, value)
	}
}

// EchoCourses stands in for canvas_list_courses and counts how often it really runs.
type EchoCourses struct {
	tools.Spec
	mu    sync.Mutex
	Calls int
}

func NewEchoCourses() *EchoCourses {
	return &EchoCourses{Spec: tools.Spec{ToolName: "canvas_list_courses", ToolDescription: "List courses", Output: "any"}}
}

func (t *EchoCourses) Forward(ctx context.Context, args tools.Args) (any, error) {
	t.mu.Lock()
	t.Calls++
	t.mu.Unlock()
	return tools.OK("Course A (ID 1), Course B (ID 2)"), nil
}

// toolChunk is one streamed tool-call delta (index, id, name, arguments); "" id/name mean absent.
func toolChunk(index int, id, name, arguments string) *openai.ChatCompletionChunk {
	delta := openai.ChatCompletionChunkChoiceDeltaToolCall{Index: int64(index), ID: id,
		Function: openai.ChatCompletionChunkChoiceDeltaToolCallFunction{Name: name, Arguments: arguments}}
	if id != "" {
		delta.Type = "function"
	}
	return &openai.ChatCompletionChunk{Choices: []openai.ChatCompletionChunkChoice{
		{Delta: openai.ChatCompletionChunkChoiceDelta{ToolCalls: []openai.ChatCompletionChunkChoiceDeltaToolCall{delta}}}}}
}

// usageChunk is the last chunk of a turn: token usage, no choices.
func usageChunk() *openai.ChatCompletionChunk {
	return &openai.ChatCompletionChunk{Usage: openai.CompletionUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}
}

// stepLimitTurns makes the agent (MaxSteps 5 in these tests) use every step on tool calls without answering.
func stepLimitTurns(prefix string) [][]*openai.ChatCompletionChunk {
	var turns [][]*openai.ChatCompletionChunk
	for i := 1; i <= 5; i++ {
		turns = append(turns, toolTurn(fmt.Sprintf("%s%d", prefix, i)))
	}
	return turns
}

func answerTurn(callID, answerJSON string) []*openai.ChatCompletionChunk {
	return []*openai.ChatCompletionChunk{toolChunk(0, callID, "final_answer_tool", answerJSON), usageChunk()}
}

func toolTurn(callID string) []*openai.ChatCompletionChunk {
	return []*openai.ChatCompletionChunk{toolChunk(0, callID, "canvas_list_courses", "{}"), usageChunk()}
}

// SimulatedCompletions plays back one scripted model turn per request and records every request.
type SimulatedCompletions struct {
	mu     sync.Mutex
	Script [][]*openai.ChatCompletionChunk
	Calls  []map[string]any
	// FinalText answers the one non-streamed call: the final answer written after the agent runs out of steps
	FinalText string
}

func (f *SimulatedCompletions) Add(turns ...[]*openai.ChatCompletionChunk) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Script = append(f.Script, turns...)
}

func (f *SimulatedCompletions) Remaining() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Script)
}

func (f *SimulatedCompletions) CreateCompletion(ctx context.Context, params map[string]any) (*openai.ChatCompletion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, params)
	if f.FinalText == "" {
		return nil, errors.New("the agent should stream")
	}
	return &openai.ChatCompletion{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{Content: f.FinalText}}}}, nil
}

func (f *SimulatedCompletions) CreateCompletionStream(ctx context.Context, params map[string]any, yield func(*openai.ChatCompletionChunk) error) error {
	f.mu.Lock()
	f.Calls = append(f.Calls, params)
	if params["stream"] != true {
		f.mu.Unlock()
		return errors.New("the agent should stream")
	}
	if _, ok := params["stop"]; ok {
		f.mu.Unlock()
		return errors.New("reasoning models must not receive `stop`")
	}
	if len(f.Script) == 0 {
		f.mu.Unlock()
		return errors.New("the simulated model has no scripted turn left")
	}
	turn := f.Script[0]
	f.Script = f.Script[1:]
	f.mu.Unlock()
	for _, chunk := range turn {
		if err := yield(chunk); err != nil {
			return err
		}
	}
	return nil
}

// LastMessages is the "messages" of the last request.
func (f *SimulatedCompletions) LastMessages() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Calls[len(f.Calls)-1]["messages"].([]map[string]any)
}

// makeAgent builds the configured agent type around a scripted model and one tool.
func makeAgent(script [][]*openai.ChatCompletionChunk, tool tools.Tool) (*agent.Agent, *SimulatedCompletions) {
	completions := &SimulatedCompletions{Script: script}
	model := models.NewOpenAIServerModel("gpt-6-luna", completions, map[string]any{"reasoning_effort": "none"})
	if tool == nil {
		tool = NewEchoCourses()
	}
	turns := 10 // the config's max_history_turns
	a, err := agent.Build("general_agent", agent.Options{
		Tools: []tools.Tool{tool}, Model: model, MaxSteps: 5, StreamOutputs: true, MaxHistoryTurns: &turns,
		Name: "canvas_student_agent", Description: "test",
	})
	if err != nil {
		panic(err)
	}
	return a, completions
}

func collect(a *agent.Agent, query string, reset bool) ([]agent.Event, error) {
	var events []agent.Event
	err := agent.StreamAgentEvents(context.Background(), a, query, reset, "", func(e agent.Event) error {
		events = append(events, e)
		return nil
	})
	return events, err
}
