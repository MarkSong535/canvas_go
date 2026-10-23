package tests

// The real SDK client against a simulated OpenAI server: the agent's request body passes through unchanged, streamed
// chunks are parsed, and the vector store tools speak the Assistants API.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"canvas_go/internal/agent"
	"canvas_go/internal/canvastools"
	"canvas_go/internal/models"
	"canvas_go/internal/tools"
)

type simulatedOpenAI struct {
	mu       sync.Mutex
	requests []map[string]any
	headers  []http.Header
}

func (f *simulatedOpenAI) record(r *http.Request) map[string]any {
	var body map[string]any
	data, _ := io.ReadAll(r.Body)
	json.Unmarshal(data, &body)
	f.mu.Lock()
	f.requests = append(f.requests, body)
	f.headers = append(f.headers, r.Header.Clone())
	f.mu.Unlock()
	return body
}

func (f *simulatedOpenAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := f.record(r)
	switch r.URL.Path {
	case "/chat/completions":
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, event := range []string{
				`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"final_answer_tool","arguments":""}}]}}]}`,
				`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"answer\": \"Hi from"}}]}}]}`,
				`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":" the SDK\"}"}}]}}]}`,
				`{"id":"c","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19}}`,
			} {
				fmt.Fprintf(w, "data: %s\n\n", event)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"standard"}}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
	case "/vector_stores/vs_1/search":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"vector_store.search_results.page","data":[{"file_id":"f1","filename":"syllabus.pdf","score":0.875,"attributes":{"week":3},"content":[{"type":"text","text":"Grading: 40% labs"}]}],"has_more":false}`)
	default:
		http.NotFound(w, r)
	}
}

func TestSDKClientStreamsTheAgentsOwnRequest(t *testing.T) {
	simulated := &simulatedOpenAI{}
	server := httptest.NewServer(simulated)
	defer server.Close()
	retries := 0
	client := &models.SDKClient{Client: models.NewSDKClient(models.ClientConfig{APIKey: "sk-test", BaseURL: server.URL, MaxRetries: &retries})}
	model := models.NewOpenAIServerModel("gpt-6-luna", client, map[string]any{"reasoning_effort": "none"})
	a, err := agent.Build("general_agent", agent.Options{Tools: []tools.Tool{NewEchoCourses()}, Model: model, MaxSteps: 3, StreamOutputs: true})
	if err != nil {
		t.Fatal(err)
	}
	var deltas strings.Builder
	var final agent.Event
	err = agent.StreamAgentEvents(context.Background(), a, "hello", true, "", func(e agent.Event) error {
		if e["type"] == "delta" {
			deltas.WriteString(e["text"].(string))
		}
		if e["type"] == "final" {
			final = e
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if final["answer"] != "Hi from the SDK" || deltas.String() != "Hi from the SDK" {
		t.Fatalf("final %v, deltas %q", final, deltas.String())
	}
	request := simulated.requests[0]
	if request["model"] != "gpt-6-luna" || request["reasoning_effort"] != "none" || request["stream"] != true ||
		request["tool_choice"] != "required" || request["stop"] != nil {
		t.Fatalf("request body: %v", request)
	}
	if model.LastInputTokenCount != 12 || model.LastOutputTokenCount != 7 {
		t.Fatalf("usage %d/%d", model.LastInputTokenCount, model.LastOutputTokenCount)
	}

	// A non-streamed call (the router's classifier) goes through the same client
	reply, err := model.Generate(context.Background(), []*models.ChatMessage{{Role: models.RoleUser, Content: models.Text("tier?")}},
		models.GenerateOptions{Extra: map[string]any{"reasoning_effort": "minimal"}})
	if err != nil || reply.ContentText() != "standard" {
		t.Fatalf("reply %v, err %v", reply, err)
	}
	if last := simulated.requests[len(simulated.requests)-1]; last["reasoning_effort"] != "minimal" || last["stream"] != nil {
		t.Fatalf("request body: %v", last)
	}
}

func TestVectorStoreSearchUsesTheAssistantsAPI(t *testing.T) {
	simulated := &simulatedOpenAI{}
	server := httptest.NewServer(simulated)
	defer server.Close()
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("OPENAI_BASE_URL", server.URL)
	out := output(t, canvastools.NewVectorStoreSearch(), map[string]any{"vector_store_id": "vs_1", "query": "grading", "max_results": 3})
	for _, want := range []string{"Relevance: 87.50%", "Source: syllabus.pdf", `Attributes: {"week":3}`, "Grading: 40% labs"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if simulated.requests[0]["query"] != "grading" || simulated.requests[0]["max_num_results"] != float64(3) {
		t.Fatalf("request: %v", simulated.requests[0])
	}
	if simulated.headers[0].Get("OpenAI-Beta") != "assistants=v2" || simulated.headers[0].Get("Authorization") != "Bearer sk-test" {
		t.Fatalf("headers: %v", simulated.headers[0])
	}
}
