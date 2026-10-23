// Package agent is the ReAct tool-calling agent, per-question model
// routing, and the UI event stream built on top of it.
package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

	"canvas_go/internal/agenterr"
	"canvas_go/internal/jsonval"
	"canvas_go/internal/logger"
	"canvas_go/internal/memory"
	"canvas_go/internal/models"
	"canvas_go/internal/tools"
)

// StreamEvent is one of *models.ChatMessageStreamDelta, *models.ChatMessageToolCall, *memory.PlanningStep,
// *memory.ActionStep or *memory.FinalAnswerStep.
type StreamEvent any

// ToolOutput is the result of processing a step's tool calls (internal to a step).
type ToolOutput struct {
	Output        any
	IsFinalAnswer bool
}

// RunResult holds extended information about a run.
type RunResult struct {
	Output     any
	State      string // "success" | "max_steps_error"
	Messages   []any
	TokenUsage *logger.TokenUsage
	Timing     logger.Timing
}

// StepCallback is called with every finished action step.
type StepCallback func(step *memory.ActionStep, agent *Agent)

// FinalAnswerCheck validates a final answer before it is accepted.
type FinalAnswerCheck func(answer any, mem *memory.AgentMemory) error

// Options configure an agent.
type Options struct {
	Tools             []tools.Tool
	Model             models.Model
	PromptTemplates   PromptTemplates // nil: loaded from TemplatePath (or the built-in prompts)
	TemplatePath      string
	MaxSteps          int  // default 20
	MaxHistoryTurns   *int // nil keeps every earlier turn
	PlanningInterval  int  // 0: no planning steps
	Name              string
	Description       string
	StreamOutputs     bool
	StepCallbacks     []StepCallback
	FinalAnswerChecks []FinalAnswerCheck
	Logger            *logger.AgentLogger
}

// Agent solves tasks step by step: the model calls tools, observes the results, and finally calls
// final_answer_tool.
type Agent struct {
	Name              string
	Description       string
	Model             models.Model
	PromptTemplates   PromptTemplates
	MaxSteps          int
	MaxHistoryTurns   *int
	PlanningInterval  int
	StreamOutputs     bool
	State             map[string]any
	Tools             []tools.Tool // in declaration order; final_answer_tool last
	toolByName        map[string]tools.Tool
	Memory            *memory.AgentMemory
	Logger            *logger.AgentLogger
	Monitor           *logger.Monitor
	StepCallbacks     []StepCallback
	FinalAnswerChecks []FinalAnswerCheck

	// Routing (set by the entry points; nil means a single model)
	Router       *ModelRouter
	RoutingState *ConversationRouting

	Task         string
	SystemPrompt string
	UserPrompt   string
	StepNumber   int
	interrupted  atomic.Bool
	mu           sync.Mutex // one run at a time
}

// New builds an agent.
func New(opts Options) (*Agent, error) {
	if opts.Name != "" && !tools.IsValidName(opts.Name) {
		return nil, fmt.Errorf("invalid agent name %q: use 1-64 letters, digits, '_' or '-'", opts.Name)
	}
	if opts.Model == nil {
		return nil, errors.New("an agent needs a model")
	}
	templates := opts.PromptTemplates
	if templates == nil {
		var err error
		if templates, err = LoadPromptTemplates(opts.TemplatePath); err != nil {
			return nil, err
		}
	}
	a := &Agent{
		Name: opts.Name, Description: opts.Description, Model: opts.Model, PromptTemplates: templates,
		MaxSteps: opts.MaxSteps, MaxHistoryTurns: opts.MaxHistoryTurns, PlanningInterval: opts.PlanningInterval,
		StreamOutputs: opts.StreamOutputs, State: map[string]any{}, toolByName: map[string]tools.Tool{},
		FinalAnswerChecks: opts.FinalAnswerChecks, Logger: opts.Logger,
	}
	if a.MaxSteps == 0 {
		a.MaxSteps = 20
	}
	if a.Logger == nil {
		a.Logger = logger.Default
	}
	names := map[string]int{}
	for _, t := range opts.Tools {
		if err := tools.Validate(t); err != nil {
			return nil, err
		}
		names[t.Name()]++
		if _, dup := a.toolByName[t.Name()]; !dup {
			a.Tools = append(a.Tools, t)
		}
		a.toolByName[t.Name()] = t
	}
	if a.Name != "" {
		names[a.Name]++
	}
	var dups []string
	for name, n := range names {
		if n > 1 {
			dups = append(dups, name)
		}
	}
	if len(dups) > 0 {
		return nil, fmt.Errorf("Each tool or managed_agent should have a unique name! You passed these duplicate names: %v", dups)
	}
	if _, ok := a.toolByName["final_answer_tool"]; !ok {
		final := tools.NewFinalAnswerTool()
		a.Tools = append(a.Tools, final)
		a.toolByName[final.Name()] = final
	}
	if a.StreamOutputs {
		if _, ok := a.Model.(models.StreamingModel); !ok {
			return nil, errors.New("StreamOutputs is on, but the model can't stream (it doesn't implement models.StreamingModel)")
		}
	}
	var err error
	if a.SystemPrompt, err = a.initializeSystemPrompt(); err != nil {
		return nil, err
	}
	if a.UserPrompt, err = a.initializeUserPrompt(); err != nil {
		return nil, err
	}
	a.Memory = memory.NewAgentMemory(a.SystemPrompt, &a.UserPrompt)
	a.Monitor = logger.NewMonitor(a.Model, a.Logger)
	a.StepCallbacks = append(opts.StepCallbacks, func(step *memory.ActionStep, _ *Agent) {
		a.Monitor.UpdateMetrics(&step.Timing, step.TokenUsage)
	})
	return a, nil
}

// Builder makes an agent of one registered type.
type Builder func(opts Options) (*Agent, error)

// Registry maps agent type names (as in the config's Type) to builders.
var Registry = map[string]Builder{"general_agent": New}

// Build makes an agent of the given registered type.
func Build(typ string, opts Options) (*Agent, error) {
	builder, ok := Registry[typ]
	if !ok {
		return nil, fmt.Errorf("%s is not in the agent registry", typ)
	}
	return builder(opts)
}

func (a *Agent) template(keys ...string) string {
	s, _ := a.PromptTemplates.Get(keys...)
	return s
}

func (a *Agent) initializeSystemPrompt() (string, error) {
	return PopulateTemplate(a.template("system_prompt"), map[string]any{"tools": toolViews(a.Tools), "managed_agents": []toolView{}})
}

func (a *Agent) initializeUserPrompt() (string, error) {
	return PopulateTemplate(a.template("user_prompt"), map[string]any{})
}

func (a *Agent) initializeTaskInstruction() (string, error) {
	return PopulateTemplate(a.template("task_instruction"), map[string]any{"task": a.Task})
}

// Tool returns a tool by name.
func (a *Agent) Tool(name string) (tools.Tool, bool) {
	t, ok := a.toolByName[name]
	return t, ok
}

// Interrupt stops the agent before its next step.
func (a *Agent) Interrupt() { a.interrupted.Store(true) }

// RunStream runs the task and passes every stream event to yield as it happens. reset=false keeps the
// conversation going from the previous run. maxSteps 0 uses the agent's default.
func (a *Agent) RunStream(ctx context.Context, task string, reset bool, maxSteps int, yield func(StreamEvent) error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if maxSteps == 0 {
		maxSteps = a.MaxSteps
	}
	a.Task = task
	a.interrupted.Store(false)
	var err error
	if a.Task, err = a.initializeTaskInstruction(); err != nil {
		return err
	}
	if a.SystemPrompt, err = a.initializeSystemPrompt(); err != nil {
		return err
	}
	a.Memory.SystemPrompt = &memory.SystemPromptStep{SystemPrompt: a.SystemPrompt}
	if a.UserPrompt, err = a.initializeUserPrompt(); err != nil {
		return err
	}
	a.Memory.UserPrompt = &memory.UserPromptStep{UserPrompt: a.UserPrompt}

	if reset {
		a.Memory.Reset()
		a.Monitor.Reset()
	} else if a.MaxHistoryTurns != nil {
		// Keep the previous `max_history_turns` tasks; the new one is appended below
		a.Memory.KeepLastTasks(*a.MaxHistoryTurns)
	}
	a.Logger.LogTask(logger.Info, strings.TrimSpace(a.Task), fmt.Sprintf("%s - %s", modelTypeName(a.Model), a.Model.ModelID()), a.Name)
	a.Memory.Steps = append(a.Memory.Steps, &memory.TaskStep{Task: a.Task})
	return a.runStream(ctx, a.Task, maxSteps, yield)
}

// Run runs the task to completion and returns the final answer.
func (a *Agent) Run(ctx context.Context, task string, reset bool) (any, error) {
	var final any
	err := a.RunStream(ctx, task, reset, 0, func(event StreamEvent) error {
		if step, ok := event.(*memory.FinalAnswerStep); ok {
			final = step.Output
		}
		return nil
	})
	return final, err
}

// RunFull is Run with token usage, timing and the full memory.
func (a *Agent) RunFull(ctx context.Context, task string, reset bool) (*RunResult, error) {
	start := logger.Now()
	output, err := a.Run(ctx, task, reset)
	if err != nil {
		return nil, err
	}
	result := &RunResult{Output: output, State: "success", Messages: a.Memory.GetFullSteps(), Timing: logger.Timing{StartTime: start}}
	result.Timing.End()
	in, out, complete := 0, 0, true
	for _, step := range a.Memory.Steps {
		var usage *logger.TokenUsage
		switch s := step.(type) {
		case *memory.ActionStep:
			usage = s.TokenUsage
		case *memory.PlanningStep:
			usage = s.TokenUsage
		default:
			continue
		}
		if usage == nil {
			complete = false
			break
		}
		in += usage.InputTokens
		out += usage.OutputTokens
	}
	if complete {
		result.TokenUsage = logger.NewTokenUsage(in, out)
	}
	if n := len(a.Memory.Steps); n > 0 {
		if s, ok := a.Memory.Steps[n-1].(*memory.ActionStep); ok && agenterr.IsType(s.Error, agenterr.TypeAgentMaxStepsError) {
			result.State = "max_steps_error"
		}
	}
	return result, nil
}

func modelTypeName(m any) string {
	t := reflect.TypeOf(m)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Name()
}

func (a *Agent) runStream(ctx context.Context, task string, maxSteps int, yield func(StreamEvent) error) error {
	a.StepNumber = 1
	returnedFinalAnswer := false
	var finalAnswer any
	var actionStep *memory.ActionStep
	for !returnedFinalAnswer && a.StepNumber <= maxSteps {
		if a.interrupted.Load() {
			return agenterr.New(agenterr.TypeAgentError, "Agent interrupted.", a.Logger)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// Run a planning step if scheduled
		if a.PlanningInterval > 0 && (a.StepNumber == 1 || (a.StepNumber-1)%a.PlanningInterval == 0) {
			start := logger.Now()
			planning, err := a.generatePlanningStep(ctx, task, len(a.Memory.Steps) == 1, a.StepNumber, yield)
			if err != nil {
				return err
			}
			a.Memory.Steps = append(a.Memory.Steps, planning)
			planning.Timing = logger.Timing{StartTime: start}
			planning.Timing.End()
			if err := yield(planning); err != nil {
				return err
			}
		}

		actionStep = &memory.ActionStep{StepNumber: a.StepNumber, Timing: logger.Timing{StartTime: logger.Now()}}
		a.Logger.LogRule(logger.Info, fmt.Sprintf("Step %d", a.StepNumber))
		stepErr := a.stepStream(ctx, actionStep, func(event any) error {
			if out, ok := event.(*ToolOutput); ok {
				if out.IsFinalAnswer {
					if err := a.validateFinalAnswer(out.Output); err != nil {
						return err
					}
					returnedFinalAnswer = true
					actionStep.IsFinalAnswer = true
					finalAnswer = out.Output
				}
				return nil
			}
			return yield(event)
		})
		var fatal error
		if stepErr != nil {
			if agentErr, ok := agenterr.As(stepErr); ok && agentErr.Type != agenterr.TypeAgentGenerationError {
				// Other AgentError types are caused by the model: record them and iterate
				actionStep.Error = stepErr
			} else {
				// Generation errors (and anything that isn't an AgentError) end the run
				fatal = stepErr
			}
		}
		a.finalizeStep(actionStep)
		a.Memory.Steps = append(a.Memory.Steps, actionStep)
		if err := yield(actionStep); err != nil {
			return err
		}
		a.StepNumber++
		if fatal != nil {
			return fatal
		}
	}
	if !returnedFinalAnswer && a.StepNumber == maxSteps+1 {
		answer, err := a.handleMaxStepsReached(ctx, task)
		if err != nil {
			return err
		}
		finalAnswer = answer
		if err := yield(actionStep); err != nil {
			return err
		}
	}
	return yield(&memory.FinalAnswerStep{Output: finalAnswer})
}

func (a *Agent) validateFinalAnswer(answer any) error {
	for i, check := range a.FinalAnswerChecks {
		if err := check(answer, a.Memory); err != nil {
			return agenterr.New(agenterr.TypeAgentError, fmt.Sprintf("Check %d failed with error: %v", i, err), a.Logger)
		}
	}
	return nil
}

func (a *Agent) finalizeStep(step *memory.ActionStep) {
	step.Timing.End()
	for _, callback := range a.StepCallbacks {
		callback(step, a)
	}
}

func (a *Agent) handleMaxStepsReached(ctx context.Context, task string) (any, error) {
	start := logger.Now()
	answer := a.ProvideFinalAnswer(ctx, task)
	step := &memory.ActionStep{
		StepNumber: a.StepNumber,
		Error:      agenterr.New(agenterr.TypeAgentMaxStepsError, "Reached max steps.", a.Logger),
		Timing:     logger.Timing{StartTime: start},
		TokenUsage: answer.TokenUsage,
	}
	step.Timing.End()
	step.ActionOutput = answer.Content
	a.finalizeStep(step)
	a.Memory.Steps = append(a.Memory.Steps, step)
	return answer.ContentText(), nil
}

// ProvideFinalAnswer asks the model for an answer from the agent's memory after it ran out of steps.
func (a *Agent) ProvideFinalAnswer(ctx context.Context, task string) *models.ChatMessage {
	messages := []*models.ChatMessage{{Role: models.RoleSystem, Content: models.Text(a.template("final_answer", "pre_messages"))}}
	memoryMessages := a.WriteMemoryToMessages(false)
	if len(memoryMessages) > 0 {
		messages = append(messages, memoryMessages[1:]...)
	}
	post, err := PopulateTemplate(a.template("final_answer", "post_messages"), map[string]any{"task": task})
	if err == nil {
		messages = append(messages, &models.ChatMessage{Role: models.RoleUser, Content: models.Text(post)})
		var reply *models.ChatMessage
		if reply, err = a.Model.Generate(ctx, messages, models.GenerateOptions{}); err == nil {
			return reply
		}
	}
	return &models.ChatMessage{Role: models.RoleAssistant, Content: fmt.Sprintf("Error in generating final LLM output:\n%v", err)}
}

func (a *Agent) generatePlanningStep(ctx context.Context, task string, isFirstStep bool, step int, yield func(StreamEvent) error) (*memory.PlanningStep, error) {
	var inputMessages []*models.ChatMessage
	views := toolViews(a.Tools)
	if isFirstStep {
		text, err := PopulateTemplate(a.template("planning", "initial_plan"), map[string]any{"task": task, "tools": views, "managed_agents": []toolView{}})
		if err != nil {
			return nil, err
		}
		inputMessages = []*models.ChatMessage{{Role: models.RoleUser, Content: models.Text(text)}}
	} else {
		// Summary mode removes the system prompt and previous planning messages output by the model
		memoryMessages := a.WriteMemoryToMessages(true)
		pre, err := PopulateTemplate(a.template("planning", "update_plan_pre_messages"), map[string]any{"task": task})
		if err != nil {
			return nil, err
		}
		post, err := PopulateTemplate(a.template("planning", "update_plan_post_messages"), map[string]any{
			"task": task, "tools": views, "managed_agents": []toolView{}, "remaining_steps": a.MaxSteps - step})
		if err != nil {
			return nil, err
		}
		inputMessages = append([]*models.ChatMessage{{Role: models.RoleSystem, Content: models.Text(pre)}}, memoryMessages[:max(0, len(memoryMessages)-1)]...)
		inputMessages = append(inputMessages, &models.ChatMessage{Role: models.RoleUser, Content: models.Text(post)})
	}
	var content strings.Builder
	inputTokens, outputTokens := 0, 0
	opts := models.GenerateOptions{StopSequences: []string{"<end_plan>"}}
	if streaming, ok := a.Model.(models.StreamingModel); ok && a.StreamOutputs {
		err := streaming.GenerateStream(ctx, inputMessages, opts, func(event *models.ChatMessageStreamDelta) error {
			content.WriteString(event.Content)
			if event.TokenUsage != nil {
				outputTokens += event.TokenUsage.OutputTokens
				inputTokens = event.TokenUsage.InputTokens
			}
			return yield(event)
		})
		if err != nil {
			return nil, err
		}
	} else {
		reply, err := a.Model.Generate(ctx, inputMessages, opts)
		if err != nil {
			return nil, err
		}
		content.WriteString(reply.ContentText())
		if reply.TokenUsage != nil {
			inputTokens, outputTokens = reply.TokenUsage.InputTokens, reply.TokenUsage.OutputTokens
		}
	}
	var plan, headline string
	if isFirstStep {
		plan = "Here are the facts I know and the plan of action that I will follow to solve the task:\n```\n" + content.String() + "\n```"
		headline = "Initial plan"
	} else {
		plan = "I still need to solve the task I was given:\n```\n" + a.Task + "\n```\n\nHere are the facts I know and my new/updated plan of action to solve the task:\n```\n" + content.String() + "\n```"
		headline = "Updated plan"
	}
	a.Logger.LogText(logger.Info, logger.Rule(headline, "─")+"\n"+plan)
	return &memory.PlanningStep{
		ModelInputMessages: inputMessages,
		Plan:               plan,
		ModelOutputMessage: &models.ChatMessage{Role: models.RoleAssistant, Content: content.String()},
		TokenUsage:         logger.NewTokenUsage(inputTokens, outputTokens),
	}, nil
}

// WriteMemoryToMessages replays the memory as model input.
func (a *Agent) WriteMemoryToMessages(summaryMode bool) []*models.ChatMessage {
	messages := a.Memory.SystemPrompt.ToMessages(summaryMode)
	for _, step := range a.Memory.Steps {
		messages = append(messages, step.ToMessages(summaryMode)...)
	}
	if a.Memory.UserPrompt != nil {
		messages = append(messages, a.Memory.UserPrompt.ToMessages(summaryMode)...)
	}
	return messages
}

// stepStream performs one ReAct step: the model thinks and calls tools, the tools run, results are recorded.
// It emits stream deltas and tool calls, then *ToolOutput values.
func (a *Agent) stepStream(ctx context.Context, step *memory.ActionStep, emit func(any) error) error {
	inputMessages := a.WriteMemoryToMessages(false)
	step.ModelInputMessages = inputMessages
	opts := models.GenerateOptions{StopSequences: []string{"Observation:", "Calling tools:"}, ToolsToCallFrom: a.Tools}

	var chatMessage *models.ChatMessage
	var genErr error
	var consumerErr error
	if streaming, ok := a.Model.(models.StreamingModel); ok && a.StreamOutputs {
		var deltas []*models.ChatMessageStreamDelta
		genErr = streaming.GenerateStream(ctx, inputMessages, opts, func(event *models.ChatMessageStreamDelta) error {
			deltas = append(deltas, event)
			// Deltas go to the caller (CLI / WebSocket) to render; the agent itself only accumulates them
			if err := emit(event); err != nil {
				consumerErr = err
				return err
			}
			return nil
		})
		if consumerErr != nil {
			return consumerErr
		}
		if genErr == nil {
			chatMessage, genErr = models.AgglomerateStreamDeltas(deltas, models.RoleAssistant)
		}
	} else {
		chatMessage, genErr = a.Model.Generate(ctx, inputMessages, opts)
	}
	if genErr != nil {
		return agenterr.Wrap(agenterr.TypeAgentGenerationError, fmt.Sprintf("Error while generating output:\n%v", genErr), genErr, a.Logger)
	}
	logContent := chatMessage.ContentText()
	if logContent == "" {
		if chatMessage.Raw != nil {
			logContent = fmt.Sprint(chatMessage.Raw)
		} else {
			logContent = fmt.Sprint(chatMessage.ToolCalls)
		}
	}
	a.Logger.LogMarkdown(logger.Debug, "Output message of the LLM:", logContent)
	step.ModelOutputMessage = chatMessage
	if text, ok := chatMessage.Content.(string); ok {
		step.ModelOutput = &text
	}
	step.TokenUsage = chatMessage.TokenUsage

	if len(chatMessage.ToolCalls) == 0 {
		var err error
		if parser, ok := a.Model.(models.ToolCallParser); ok {
			chatMessage, err = parser.ParseToolCalls(chatMessage)
		} else {
			chatMessage, err = models.ParseToolCalls(chatMessage, "name", "arguments")
		}
		if err != nil {
			return agenterr.New(agenterr.TypeAgentParsingError, fmt.Sprintf("Error while parsing tool call from model output: %v", err), a.Logger)
		}
	} else {
		for i := range chatMessage.ToolCalls {
			chatMessage.ToolCalls[i].Function.Arguments = models.ParseJSONIfNeeded(chatMessage.ToolCalls[i].Function.Arguments)
		}
	}
	return a.processToolCalls(ctx, chatMessage, step, emit)
}

type pendingCall struct {
	name      string
	arguments any
	id        string
}

// processToolCalls runs the model's tool calls (concurrently when there are several) and records the results.
func (a *Agent) processToolCalls(ctx context.Context, chatMessage *models.ChatMessage, step *memory.ActionStep, emit func(any) error) error {
	var modelOutputs []string
	var calls []memory.ToolCall
	var parallel []pendingCall
	var finalCall *pendingCall
	for i := range chatMessage.ToolCalls {
		tc := &chatMessage.ToolCalls[i]
		if err := emit(tc); err != nil {
			return err
		}
		modelOutputs = append(modelOutputs, fmt.Sprintf("Called Tool: '%s' with arguments: %s", tc.Function.Name, jsonval.Text(tc.Function.Arguments)))
		calls = append(calls, memory.ToolCall{Name: tc.Function.Name, Arguments: tc.Function.Arguments, ID: tc.ID})
		if tc.Function.Name == "final_answer_tool" {
			finalCall = &pendingCall{tc.Function.Name, tc.Function.Arguments, tc.ID}
			break // Stop: final answer reached, no further tool calls
		}
		parallel = append(parallel, pendingCall{tc.Function.Name, tc.Function.Arguments, tc.ID})
	}
	// Record the calls before running them, so a failing tool still leaves a complete call/result pair in memory
	if len(modelOutputs) > 0 {
		joined := strings.Join(modelOutputs, "\n")
		step.ModelOutput = &joined
	}
	if len(calls) > 0 {
		step.ToolCalls = calls
	}
	step.ToolResults = map[string]string{}
	var resultsMu sync.Mutex

	runOne := func(call pendingCall) (string, error) {
		a.Logger.LogPanel(logger.Info, "", fmt.Sprintf("Calling tool: '%s' with arguments: %s", call.name, jsonval.Text(call.arguments)))
		arguments := call.arguments
		if arguments == nil {
			arguments = map[string]any{}
		}
		if previous, ok := a.previousToolResult(call.name, arguments); ok {
			// Same call already answered in this task: don't hit the API again, and tell the model so
			observation := fmt.Sprintf("%s\n\n(You already called %s with exactly these arguments for this question; "+
				"this is the same result. Do not call it again: use it, or answer with final_answer_tool.)", previous, call.name)
			resultsMu.Lock()
			step.ToolResults[call.id] = observation
			resultsMu.Unlock()
			return observation, nil
		}
		result, err := a.ExecuteToolCall(ctx, call.name, arguments)
		if err != nil {
			return "", err
		}
		observation := strings.TrimSpace(jsonval.Text(result))
		a.Logger.Log(logger.Info, "Observations: "+strings.ReplaceAll(observation, "[", "|"))
		resultsMu.Lock()
		step.ToolResults[call.id] = observation
		resultsMu.Unlock()
		return observation, nil
	}

	var observations []string
	if len(parallel) == 1 {
		observation, err := runOne(parallel[0])
		if err != nil {
			return err
		}
		observations = append(observations, observation)
		if err := emit(&ToolOutput{}); err != nil {
			return err
		}
	} else if len(parallel) > 1 {
		results := make([]string, len(parallel))
		errs := make([]error, len(parallel))
		var wg sync.WaitGroup
		for i, call := range parallel {
			wg.Add(1)
			go func(i int, call pendingCall) {
				defer wg.Done()
				results[i], errs[i] = runOne(call)
			}(i, call)
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				return err
			}
		}
		observations = append(observations, results...)
		for range results {
			if err := emit(&ToolOutput{}); err != nil {
				return err
			}
		}
	}

	if finalCall != nil {
		a.Logger.LogPanel(logger.Info, "", fmt.Sprintf("Calling tool: '%s' with arguments: %s", finalCall.name, jsonval.Text(finalCall.arguments)))
		answer := finalCall.arguments
		if m, ok := finalCall.arguments.(map[string]any); ok {
			if v, has := m["answer"]; has {
				answer = v
			}
		}
		var finalAnswer any
		if s, ok := answer.(string); ok && a.State[s] != nil {
			// The answer names a state variable: return its value
			finalAnswer = a.State[s]
			a.Logger.Log(logger.Info, fmt.Sprintf("Final answer: Extracting key '%s' from state to return value '%v'.", s, finalAnswer))
		} else {
			var err error
			if finalAnswer, err = a.ExecuteToolCall(ctx, "final_answer_tool", finalCall.arguments); err != nil {
				return err
			}
			a.Logger.LogText(logger.Info, "Final answer: "+jsonval.Text(finalAnswer))
		}
		step.ActionOutput = finalAnswer
		if err := emit(&ToolOutput{Output: finalAnswer, IsFinalAnswer: true}); err != nil {
			return err
		}
	}
	if len(observations) > 0 {
		joined := strings.Join(observations, "\n")
		step.Observations = &joined
	}
	return nil
}

// previousToolResult is the result of an identical earlier call (same tool and arguments) for the current task.
func (a *Agent) previousToolResult(name string, arguments any) (string, bool) {
	for i := len(a.Memory.Steps) - 1; i >= 0; i-- {
		switch step := a.Memory.Steps[i].(type) {
		case *memory.TaskStep:
			return "", false // only look within the current question
		case *memory.ActionStep:
			if len(step.ToolCalls) == 0 || len(step.ToolResults) == 0 {
				continue
			}
			for _, call := range step.ToolCalls {
				if result, ok := step.ToolResults[call.ID]; ok && call.Name == name && reflect.DeepEqual(normalizeArgs(call.Arguments), normalizeArgs(arguments)) {
					return strings.SplitN(result, "\n\n(You already called", 2)[0], true
				}
			}
		}
	}
	return "", false
}

func normalizeArgs(v any) any {
	if v == nil {
		return map[string]any{}
	}
	return v
}

// ExecuteToolCall runs a tool by name with the model's arguments.
func (a *Agent) ExecuteToolCall(ctx context.Context, name string, arguments any) (result any, err error) {
	tool, ok := a.toolByName[name]
	if !ok {
		names := make([]string, len(a.Tools))
		for i, t := range a.Tools {
			names[i] = t.Name()
		}
		return nil, agenterr.New(agenterr.TypeAgentToolExecutionError,
			fmt.Sprintf("Unknown tool %s, should be one of: %s.", name, strings.Join(names, ", ")), a.Logger)
	}
	arguments = a.substituteStateVariables(arguments)
	if _, isMap := arguments.(map[string]any); !isMap {
		if _, isString := arguments.(string); !isString {
			return nil, agenterr.New(agenterr.TypeAgentToolCallError, fmt.Sprintf("Unsupported arguments type: %T", arguments), a.Logger)
		}
	}
	defer func() {
		if r := recover(); r != nil {
			err = agenterr.New(agenterr.TypeAgentToolExecutionError, fmt.Sprintf(
				"Error executing tool '%s' with arguments %s: panic: %v\nPlease try again or use another tool", name, jsonval.JSON(arguments), r), a.Logger)
		}
	}()
	result, err = tools.Call(ctx, tool, arguments)
	if err != nil {
		var argErr *tools.ArgumentError
		if errors.As(err, &argErr) {
			return nil, agenterr.Wrap(agenterr.TypeAgentToolCallError, fmt.Sprintf(
				"Invalid call to tool '%s' with arguments %s: %v\nYou should call this tool with correct input arguments.\n"+
					"Expected parameters: %s\nReturns output type: %s\nTool description: '%s'",
				name, jsonval.JSON(arguments), err, jsonval.JSON(tools.ParametersJSON(tool.Parameters())), tool.OutputType(), tool.Description()), err, a.Logger)
		}
		return nil, agenterr.Wrap(agenterr.TypeAgentToolExecutionError, fmt.Sprintf(
			"Error executing tool '%s' with arguments %s: %s: %v\nPlease try again or use another tool",
			name, jsonval.JSON(arguments), errorTypeName(err), err), err, a.Logger)
	}
	return result, nil
}

func (a *Agent) substituteStateVariables(arguments any) any {
	m, ok := arguments.(map[string]any)
	if !ok {
		return arguments
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		if s, ok := v.(string); ok {
			if state, has := a.State[s]; has {
				out[k] = state
				continue
			}
		}
		out[k] = v
	}
	return out
}

func errorTypeName(err error) string {
	t := reflect.TypeOf(err)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	name := t.Name()
	if name == "" || name == "errorString" || name == "wrapError" {
		return "Error"
	}
	return name
}
