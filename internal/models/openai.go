package models

import (
	"context"
	"fmt"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"canvas_go/internal/jsonval"
	"canvas_go/internal/logger"
)

// ChatClient sends chat-completions requests. params is the full request body (model, messages, tools, ...).
// Tests replace it with a scripted simulated.
type ChatClient interface {
	CreateCompletion(ctx context.Context, params map[string]any) (*openai.ChatCompletion, error)
	CreateCompletionStream(ctx context.Context, params map[string]any, yield func(*openai.ChatCompletionChunk) error) error
}

// SDKClient sends chat-completions requests with the official OpenAI SDK (retries, timeouts, stream parsing).
// The request body is the agent's own JSON, so every chat-completions parameter passes through unchanged.
type SDKClient struct{ Client openai.Client }

// ClientConfig configures the SDK client; zero values keep the SDK defaults.
type ClientConfig struct {
	APIKey       string
	BaseURL      string
	Organization string
	Project      string
	MaxRetries   *int
	Timeout      time.Duration // per attempt
	Headers      map[string]string
}

// NewSDKClient creates an OpenAI SDK client.
func NewSDKClient(cfg ClientConfig) openai.Client {
	opts := []option.RequestOption{option.WithAPIKey(cfg.APIKey)}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.Organization != "" {
		opts = append(opts, option.WithOrganization(cfg.Organization))
	}
	if cfg.Project != "" {
		opts = append(opts, option.WithProject(cfg.Project))
	}
	if cfg.MaxRetries != nil {
		opts = append(opts, option.WithMaxRetries(*cfg.MaxRetries))
	}
	if cfg.Timeout > 0 {
		opts = append(opts, option.WithRequestTimeout(cfg.Timeout))
	}
	for k, v := range cfg.Headers {
		opts = append(opts, option.WithHeader(k, v))
	}
	return openai.NewClient(opts...)
}

// AssistantsClient is an SDK client for the vector store and file APIs (OPENAI_BASE_URL is honored).
func AssistantsClient(apiKey string) openai.Client {
	return NewSDKClient(ClientConfig{APIKey: apiKey, Headers: map[string]string{"OpenAI-Beta": "assistants=v2"}})
}

func requestBody(params map[string]any) (option.RequestOption, error) {
	body, err := jsonval.Marshal(params)
	if err != nil {
		return nil, err
	}
	return option.WithRequestBody("application/json", body), nil
}

func (c *SDKClient) CreateCompletion(ctx context.Context, params map[string]any) (*openai.ChatCompletion, error) {
	body, err := requestBody(params)
	if err != nil {
		return nil, err
	}
	return c.Client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{}, body)
}

func (c *SDKClient) CreateCompletionStream(ctx context.Context, params map[string]any, yield func(*openai.ChatCompletionChunk) error) error {
	body, err := requestBody(params)
	if err != nil {
		return err
	}
	stream := c.Client.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{}, body)
	defer stream.Close()
	for stream.Next() {
		chunk := stream.Current()
		if err := yield(&chunk); err != nil {
			return err
		}
	}
	return stream.Err()
}

// OpenAIServerModel talks to an OpenAI-compatible chat-completions API.
type OpenAIServerModel struct {
	ID                    string
	Client                ChatClient
	Params                map[string]any // default request parameters, e.g. reasoning_effort
	CustomRoleConversions map[MessageRole]MessageRole
	FlattenMessagesAsText bool
	ToolNameKey           string
	ToolArgumentsKey      string

	LastInputTokenCount  int
	LastOutputTokenCount int
}

// NewOpenAIServerModel creates a model that sends requests through client; params are default request parameters.
func NewOpenAIServerModel(modelID string, client ChatClient, params map[string]any) *OpenAIServerModel {
	if params == nil {
		params = map[string]any{}
	}
	return &OpenAIServerModel{ID: modelID, Client: client, Params: params, ToolNameKey: "name", ToolArgumentsKey: "arguments"}
}

func (m *OpenAIServerModel) ModelID() string { return m.ID }

// PrepareRequest builds the request body. Priority: Extra > specific options > model defaults.
func (m *OpenAIServerModel) PrepareRequest(messages []*ChatMessage, opts GenerateOptions) (map[string]any, error) {
	conversions := m.CustomRoleConversions
	if len(conversions) == 0 {
		conversions = ToolRoleConversions
	}
	clean, err := CleanMessageList(messages, conversions, m.FlattenMessagesAsText)
	if err != nil {
		return nil, err
	}
	params := map[string]any{}
	for k, v := range m.Params {
		params[k] = v
	}
	params["messages"] = clean
	if opts.StopSequences != nil && SupportsStopParameter(m.ID) {
		params["stop"] = opts.StopSequences
	}
	if opts.ResponseFormat != nil {
		params["response_format"] = opts.ResponseFormat
	}
	if len(opts.ToolsToCallFrom) > 0 {
		schemas := make([]any, len(opts.ToolsToCallFrom))
		for i, t := range opts.ToolsToCallFrom {
			schemas[i] = ToolJSONSchema(t)
		}
		params["tools"] = schemas
		params["tool_choice"] = "required"
	}
	params["model"] = m.ID
	for k, v := range opts.Extra {
		params[k] = v
	}
	// Reasoning models reject `stop`, even when it arrives via an explicit override
	if !SupportsStopParameter(m.ID) {
		delete(params, "stop")
	}
	return params, nil
}

func (m *OpenAIServerModel) GenerateStream(ctx context.Context, messages []*ChatMessage, opts GenerateOptions, yield func(*ChatMessageStreamDelta) error) error {
	params, err := m.PrepareRequest(messages, opts)
	if err != nil {
		return err
	}
	params["stream"] = true
	params["stream_options"] = map[string]any{"include_usage": true}
	return m.Client.CreateCompletionStream(ctx, params, func(event *openai.ChatCompletionChunk) error {
		if usage := event.Usage; usage.PromptTokens > 0 || usage.CompletionTokens > 0 {
			m.LastInputTokenCount = int(usage.PromptTokens)
			m.LastOutputTokenCount = int(usage.CompletionTokens)
			if err := yield(&ChatMessageStreamDelta{TokenUsage: logger.NewTokenUsage(int(usage.PromptTokens), int(usage.CompletionTokens))}); err != nil {
				return err
			}
		}
		if len(event.Choices) == 0 {
			return nil
		}
		choiceDelta := event.Choices[0].Delta
		delta := &ChatMessageStreamDelta{Content: choiceDelta.Content}
		for _, tc := range choiceDelta.ToolCalls {
			index := int(tc.Index)
			delta.ToolCalls = append(delta.ToolCalls, ChatMessageToolCallStreamDelta{Index: &index, ID: tc.ID, Type: tc.Type,
				Function: &ChatMessageToolCallFunction{Name: tc.Function.Name, Arguments: tc.Function.Arguments}})
		}
		return yield(delta)
	})
}

func (m *OpenAIServerModel) Generate(ctx context.Context, messages []*ChatMessage, opts GenerateOptions) (*ChatMessage, error) {
	params, err := m.PrepareRequest(messages, opts)
	if err != nil {
		return nil, err
	}
	response, err := m.Client.CreateCompletion(ctx, params)
	if err != nil {
		return nil, err
	}
	if len(response.Choices) == 0 {
		return nil, fmt.Errorf("the API returned no choices")
	}
	m.LastInputTokenCount = int(response.Usage.PromptTokens)
	m.LastOutputTokenCount = int(response.Usage.CompletionTokens)
	usage := logger.NewTokenUsage(m.LastInputTokenCount, m.LastOutputTokenCount)
	message := response.Choices[0].Message
	out := &ChatMessage{Role: RoleAssistant, Content: message.Content, Raw: response, TokenUsage: usage}
	for _, tc := range message.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ChatMessageToolCall{
			ID: tc.ID, Type: tc.Type,
			Function: ChatMessageToolCallFunction{Name: tc.Function.Name, Arguments: tc.Function.Arguments},
		})
	}
	return out, nil
}

func (m *OpenAIServerModel) ParseToolCalls(message *ChatMessage) (*ChatMessage, error) {
	return ParseToolCalls(message, m.ToolNameKey, m.ToolArgumentsKey)
}
