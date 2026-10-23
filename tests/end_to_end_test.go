package tests

// End-to-end tests for the agent, streaming, memory, the WebSocket handler and LaTeX rendering.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"

	"canvas_go/internal/agent"
	"canvas_go/internal/canvastools"
	"canvas_go/internal/chat"
	"canvas_go/internal/jsonval"
	"canvas_go/internal/latexsvg"
	"canvas_go/internal/memory"
	"canvas_go/internal/models"
	"canvas_go/internal/wsserver"
)

func mustJSON(v any) string {
	out, err := jsonval.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(out)
}

// --- models ------------------------------------------------------------------------------------------------

func TestModelTableRespectsPriceCap(t *testing.T) {
	for _, m := range models.OpenAIModels {
		if math.Max(m.InputPrice, m.OutputPrice) > models.MaxPricePer1MTokens {
			t.Fatalf("%s is above the price cap", m.ModelID)
		}
	}
	if _, err := models.Manager.InitModels(false); err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	for _, name := range models.Manager.ListModels() {
		registered[name] = true
	}
	if !registered["gpt-6-luna"] || registered["gpt-4o"] || registered["gpt-5"] {
		t.Fatalf("unexpected model set: %v", models.Manager.ListModels())
	}
}

func TestStopParameterDroppedForReasoningModels(t *testing.T) {
	for _, id := range []string{"gpt-6-luna", "gpt-5.4-mini", "gpt-5-mini", "o4-mini"} {
		if models.SupportsStopParameter(id) {
			t.Fatal(id)
		}
	}
	if !models.SupportsStopParameter("gpt-4.1-mini") {
		t.Fatal("gpt-4.1-mini supports stop")
	}
}

// --- streaming ---------------------------------------------------------------------------------------------

func TestPartialJSONDecoder(t *testing.T) {
	if _, ok := agent.PartialJSONStringValue(`{"ans`, "answer"); ok {
		t.Fatal("no value yet")
	}
	cases := map[string]string{
		`{"answer": "a\`:              "a",
		`{"answer": "a\nb\u00`:        "a\nb",
		`{"answer": "\ud83d\ude00!"}`: "\U0001F600!",
	}
	for raw, want := range cases {
		if got, ok := agent.PartialJSONStringValue(raw, "answer"); !ok || got != want {
			t.Fatalf("%q -> %q, want %q", raw, got, want)
		}
	}
}

func TestStreamingMultiTurnAndNativeToolHistory(t *testing.T) {
	answerArgs := mustJSON(map[string]any{"answer": "You have 2 courses:\n- Course A\n- \"Course B\" é\U0001F600"})
	// Escape non-ASCII as \u sequences so the stream has escapes (and surrogate pairs) to split
	answerArgs = asciiJSON(answerArgs)
	// Split the JSON at awkward points (mid-escape, mid-\u sequence) to exercise the partial decoder
	second := []*openai.ChatCompletionChunk{toolChunk(0, "call_2", "final_answer_tool", "")}
	for i := 0; i < len(answerArgs); i += 3 {
		second = append(second, toolChunk(0, "", "", answerArgs[i:min(i+3, len(answerArgs))]))
	}
	second = append(second, usageChunk())
	a, completions := makeAgent([][]*openai.ChatCompletionChunk{
		{toolChunk(0, "call_1", "canvas_list_courses", ""), toolChunk(0, "", "", "{}"), usageChunk()},
		second,
		answerTurn("call_3", mustJSON(map[string]any{"answer": "Course B is ID 2."})),
	}, nil)

	events, err := collect(a, "List my courses", false)
	if err != nil {
		t.Fatal(err)
	}
	var deltas strings.Builder
	deltaCount, sawTool := 0, false
	for _, e := range events {
		if e["type"] == "delta" {
			deltas.WriteString(e["text"].(string))
			deltaCount++
		}
		if e["type"] == "tool" && e["name"] == "canvas_list_courses" {
			sawTool = true
		}
	}
	var decoded map[string]string
	json.Unmarshal([]byte(answerArgs), &decoded)
	last := events[len(events)-1]
	if !sawTool || last["type"] != "final" || deltas.String() != decoded["answer"] || last["answer"] != decoded["answer"] {
		t.Fatalf("events: %v", events)
	}
	if deltaCount <= 10 {
		t.Fatalf("the answer should arrive in many deltas, got %d", deltaCount)
	}

	events, err = collect(a, "What's the ID of the second one?", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := events[len(events)-1]; !reflect.DeepEqual(got, agent.Event{"type": "final", "answer": "Course B is ID 2."}) {
		t.Fatalf("final: %v", got)
	}

	// The second request carries the first turn: question, tool result and answer
	messages := completions.LastMessages()
	sent := mustJSON(messages)
	for _, want := range []string{"List my courses", "Course A (ID 1)", "You have 2 courses"} {
		if !strings.Contains(sent, want) {
			t.Fatalf("missing %q in %s", want, sent)
		}
	}
	// Native tool calling: each assistant tool_calls message is followed by one `tool` message per call id
	sawToolResult := false
	for i, message := range messages {
		calls, ok := message["tool_calls"].([]any)
		if ok && len(calls) > 0 {
			for j, raw := range calls {
				call := raw.(map[string]any)
				if _, isString := call["function"].(map[string]any)["arguments"].(string); !isString {
					t.Fatal("arguments must be a JSON string")
				}
				following := messages[i+1+j]
				if following["role"] != "tool" || following["tool_call_id"] != call["id"] {
					t.Fatalf("call %v not followed by its result: %v", call["id"], following)
				}
			}
		}
		if message["role"] == "tool" && strings.Contains(mustJSON(message["content"]), "Course A (ID 1)") {
			sawToolResult = true
		}
		if strings.Contains(mustJSON(message["content"]), "Called Tool:") {
			t.Fatal("no narrated tool calls")
		}
	}
	if !sawToolResult {
		t.Fatal("the tool result should be a native tool message")
	}

	a.Memory.KeepLastTasks(1)
	tasks := 0
	for _, s := range a.Memory.Steps {
		if _, ok := s.(*memory.TaskStep); ok {
			tasks++
		}
	}
	if tasks != 1 {
		t.Fatalf("tasks after trim: %d", tasks)
	}
}

// asciiJSON escapes every non-ASCII character in a JSON text as \u sequences.
func asciiJSON(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x80:
			b.WriteRune(r)
		case r > 0xFFFF:
			r -= 0x10000
			b.WriteString(`\u` + hex4(0xD800+(r>>10)) + `\u` + hex4(0xDC00+(r&0x3FF)))
		default:
			b.WriteString(`\u` + hex4(r))
		}
	}
	return b.String()
}

func hex4(r rune) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[r>>12&0xF], digits[r>>8&0xF], digits[r>>4&0xF], digits[r&0xF]})
}

func TestRepeatedToolCallServedFromMemory(t *testing.T) {
	tool := NewEchoCourses()
	a, completions := makeAgent([][]*openai.ChatCompletionChunk{toolTurn("r1"), toolTurn("r2"), answerTurn("r3", `{"answer": "done"}`)}, tool)
	events, err := collect(a, "courses?", true)
	if err != nil {
		t.Fatal(err)
	}
	if events[len(events)-1]["answer"] != "done" {
		t.Fatalf("events: %v", events)
	}
	if tool.Calls != 1 {
		t.Fatalf("an identical repeat must not hit the API again (%d calls)", tool.Calls)
	}
	var toolMessages []map[string]any
	for _, m := range completions.LastMessages() {
		if m["role"] == "tool" {
			toolMessages = append(toolMessages, m)
		}
	}
	if !strings.Contains(mustJSON(toolMessages[len(toolMessages)-1]["content"]), "You already called canvas_list_courses") {
		t.Fatal("the repeat should be flagged to the model")
	}
}

func TestControlCharactersStripped(t *testing.T) {
	a, _ := makeAgent([][]*openai.ChatCompletionChunk{answerTurn("a1", mustJSON(map[string]any{"answer": "x\x02 = 1\n"}))}, nil)
	events, err := collect(a, "q", true)
	if err != nil {
		t.Fatal(err)
	}
	if events[len(events)-1]["answer"] != "x = 1\n" {
		t.Fatalf("answer: %q", events[len(events)-1]["answer"])
	}
	for _, e := range events {
		if text, ok := e["text"].(string); ok && strings.Contains(text, "\x02") {
			t.Fatal("control character in a delta")
		}
	}
}

// --- WebSocket handler and terminal chat ------------------------------------------------------------------

func TestWebSocketChatStreamsFramesAndResetClearsMemory(t *testing.T) {
	a, _ := makeAgent([][]*openai.ChatCompletionChunk{
		toolTurn("c1"),
		{toolChunk(0, "c2", "final_answer_tool", `{"answer": "Two co`), toolChunk(0, "", "", `urses."}`), usageChunk()},
	}, nil)
	wsserver.SetAgent(a)
	defer wsserver.SetAgent(nil)
	var frames []map[string]any
	send := func(frame wsserver.Frame) error {
		var copy map[string]any // frames must be JSON-serializable
		if err := json.Unmarshal([]byte(mustJSON(frame)), &copy); err != nil {
			return err
		}
		frames = append(frames, copy)
		return nil
	}
	ctx := context.Background()
	chatResponse, _, err := wsserver.HandleMessage(ctx, wsserver.Frame{"type": "chat", "query": "courses?"}, nil, send)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Memory.Steps) == 0 {
		t.Fatal("the conversation should be in memory")
	}
	resetResponse, _, err := wsserver.HandleMessage(ctx, wsserver.Frame{"type": "reset"}, nil, send)
	if err != nil {
		t.Fatal(err)
	}
	if got := mustJSON(chatResponse); got != mustJSON(map[string]any{"type": "answer", "answer": "Two courses.", "renders": map[string]any{}, "can_escalate": false}) {
		t.Fatalf("chat response: %s", got)
	}
	if !reflect.DeepEqual(frames[0], map[string]any{"type": "tool", "name": "canvas_list_courses"}) {
		t.Fatalf("first frame: %v", frames[0])
	}
	var deltas strings.Builder
	for _, f := range frames {
		if f["type"] == "delta" {
			deltas.WriteString(f["text"].(string))
		}
	}
	if deltas.String() != "Two courses." {
		t.Fatalf("deltas: %q", deltas.String())
	}
	if !reflect.DeepEqual(resetResponse, wsserver.Frame{"status": "reset"}) || len(a.Memory.Steps) != 0 {
		t.Fatalf("reset: %v, %d steps", resetResponse, len(a.Memory.Steps))
	}
}

func TestTerminalChatRendersAnswer(t *testing.T) {
	a, completions := makeAgent([][]*openai.ChatCompletionChunk{answerTurn("t1", `{"answer": "**Bold** reply"}`)}, nil)
	var out bytes.Buffer
	console := &chat.Console{Out: &out, In: bufio.NewReader(strings.NewReader(""))}
	console.ProcessQuery(context.Background(), a, "hi", nil)
	if completions.Remaining() != 0 {
		t.Fatal("the scripted answer should have been consumed")
	}
	if !strings.Contains(out.String(), "Bold") || !strings.Contains(out.String(), "reply") {
		t.Fatalf("output: %q", out.String())
	}
}

// --- LaTeX -------------------------------------------------------------------------------------------------

func TestLatexExtractionRules(t *testing.T) {
	text := "Inline \\(a+b\\) and $c$; prices $5 and $10; $$d=e$$ and \\[f\\]\n" +
		"code `$x$` stays literal\n```chemfig\n\\chemfig{*6(=-=-=-)}\n```\n"
	var keys []string
	for _, s := range latexsvg.Extract(text) {
		keys = append(keys, s.Key())
	}
	want := []string{"C:\\chemfig{*6(=-=-=-)}", "I:a+b", "I:c", "D:d=e", "D:f"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys %q, want %q", keys, want)
	}
}

func TestChemfigLabelsMadeFormulaSafe(t *testing.T) {
	fixed := latexsvg.FixChemfigLabels(`\arrow{->[1. NaOH][2. CH_3I]} \arrow{->[\ce{H2SO4}][\Delta]}`)
	for _, want := range []string{`\ensuremath{\mathrm{CH_3I}}`, `\ensuremath{\mathrm{\Delta}}`, `\ce{H2SO4}`, "NaOH"} {
		if !strings.Contains(fixed, want) {
			t.Fatalf("missing %q in %q", want, fixed)
		}
	}
	if got := latexsvg.FixChemfigLabels(`\chemfig{H_3C-CH_2-OH}`); got != `\chemfig{H_3C-CH_2-OH}` {
		t.Fatalf("structure changed: %q", got)
	}
}

func TestUnsafeLatexRefused(t *testing.T) {
	r := latexsvg.RenderMany(context.Background(), []latexsvg.Snippet{{Kind: "inline", Tex: `\input{/etc/passwd}`}})[0]
	if r.SVG != "" || !strings.Contains(r.Error, "not allowed") {
		t.Fatalf("result: %+v", r)
	}
}

func TestLatexRendersToSVG(t *testing.T) {
	if !latexsvg.Available() {
		t.Skip("latex/dvisvgm not installed")
	}
	scheme := "\\schemestart \\chemfig{*6(-(-OH)=-=-=-)} \\arrow{->[1. NaOH][2. CH_3I]} " +
		"\\chemfig{*6(-(-O-CH_3)=-=-=-)} \\schemestop"
	answer := "Inline \\(h(n) \\le h^*(n)\\), display $$\\frac{1}{3}$$\n\n```chemfig\n" + scheme + "\n```\n"
	renders := latexsvg.RenderAnswer(context.Background(), answer)
	want := map[string]bool{"I:h(n) \\le h^*(n)": true, "D:\\frac{1}{3}": true, "C:" + scheme: true}
	if len(renders) != len(want) {
		t.Fatalf("renders: %v", keysOf(renders))
	}
	for key, svg := range renders {
		if !want[key] || !strings.HasPrefix(svg, "<svg") || !strings.Contains(svg, "currentColor") {
			t.Fatalf("bad render for %q", key)
		}
	}
	// One bad snippet must not take the others down
	results := latexsvg.RenderMany(context.Background(), []latexsvg.Snippet{{Kind: "inline", Tex: "\\frac{1}{"}, {Kind: "inline", Tex: "x^2"}})
	if results[0].SVG != "" || !strings.Contains(results[0].Error, "unbalanced") {
		t.Fatalf("bad snippet: %+v", results[0])
	}
	if results[1].SVG == "" {
		t.Fatalf("good snippet: %+v", results[1])
	}
}

func keysOf(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestSystemPromptListsToolInputsAsJSON(t *testing.T) {
	a, _ := makeAgent(nil, canvastools.NewGetAssignments())
	prompt := a.SystemPrompt
	want := `Takes inputs: {"course_id":{"type":"string","description":"Course ID"},"include_submission":{"type":"boolean","description":"Whether to include submission information (default: true)","nullable":true}}`
	if !strings.Contains(prompt, want) {
		t.Fatalf("tool inputs should be JSON; prompt:\n%s", prompt)
	}
	if strings.Contains(prompt, "'type'") || strings.Contains(prompt, "True") {
		t.Fatal("no dict-style or capitalized-boolean syntax in the prompt")
	}
}

func TestLenientToolArguments(t *testing.T) {
	args := models.ParseJSONIfNeeded(`{answer: 'it\'s', n: 3, list: [1, 2,], // note
	}`)
	want := map[string]any{"answer": "it's", "n": 3.0, "list": []any{1.0, 2.0}}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args: %#v", args)
	}
	call, err := models.GetToolCallFromText("Action:\n{\"function\": {\"name\": \"final_answer_tool\", \"arguments\": {\"answer\": \"line\nbreak\"}}}", "name", "arguments")
	if err != nil || call.Function.Name != "final_answer_tool" || call.Function.Arguments.(map[string]any)["answer"] != "line\nbreak" {
		t.Fatalf("call %+v, err %v", call, err)
	}
	if models.ParseJSONIfNeeded("not json at all") != "not json at all" {
		t.Fatal("unparsable text stays text")
	}
}
