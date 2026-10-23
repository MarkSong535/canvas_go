package agent

/*
Turn an agent's raw stream (model deltas, tool calls, memory steps) into small UI events.

The agent answers by calling `final_answer_tool(answer=...)`, so the answer text arrives as streamed tool-call
*arguments* (partial JSON). This decodes the `answer` string out of that partial JSON as it grows, so front ends
can render the reply token by token.

Events (JSON objects):

	{"type": "route", "tier": str}      the model tier this question was routed to (only with a router)
	{"type": "tool", "name": str}       a non-final tool is being called
	{"type": "delta", "text": str}      more answer text
	{"type": "retract"}                 the answer streamed in this step was rejected; clear it
	{"type": "final", "answer": str}    authoritative final answer (always sent last)
*/

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"

	"canvas_go/internal/jsonval"
	"canvas_go/internal/memory"
	"canvas_go/internal/models"
)

const FinalAnswerTool = "final_answer_tool"

// Event is one UI event (a JSON object).
type Event = map[string]any

// C0 control characters (except tab/newline/CR) occasionally appear in model output and render as junk
var controlRE = regexp.MustCompile("[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]")

var escapes = map[byte]string{'"': `"`, '\\': `\`, '/': "/", 'b': "\b", 'f': "\f", 'n': "\n", 'r': "\r", 't': "\t"}

// PartialJSONStringValue decodes the (possibly unterminated) string value of key from a partial JSON object.
// ok is false until the key and its opening quote have arrived. Stops before an incomplete escape sequence.
func PartialJSONStringValue(raw, key string) (string, bool) {
	re := regexp.MustCompile(`"` + regexp.QuoteMeta(key) + `"\s*:\s*"`)
	loc := re.FindStringIndex(raw)
	if loc == nil {
		return "", false
	}
	var out strings.Builder
	i := loc[1]
	for i < len(raw) {
		c := raw[i]
		if c == '"' {
			break
		}
		if c != '\\' {
			out.WriteByte(c)
			i++
			continue
		}
		if i+1 >= len(raw) {
			break
		}
		esc := raw[i+1]
		if esc != 'u' {
			if s, ok := escapes[esc]; ok {
				out.WriteString(s)
			} else {
				out.WriteByte(esc)
			}
			i += 2
			continue
		}
		if i+6 > len(raw) {
			break
		}
		code, err := strconv.ParseUint(raw[i+2:i+6], 16, 32)
		if err != nil {
			break
		}
		if code >= 0xD800 && code < 0xDC00 {
			// High surrogate: wait for the low half before emitting
			if i+12 > len(raw) {
				break
			}
			if raw[i+6:i+8] == `\u` {
				if low, err := strconv.ParseUint(raw[i+8:i+12], 16, 32); err == nil {
					out.WriteRune(utf16.DecodeRune(rune(code), rune(low)))
					i += 12
					continue
				}
			}
		}
		out.WriteRune(rune(code))
		i += 6
	}
	return out.String(), true
}

type streamingCall struct {
	name string
	args strings.Builder
	sent int
}

// StreamAgentEvents runs the agent on task and emits UI events. reset=false keeps the conversation history.
//
// When the agent has a Router, the question is routed once, before the run, to a model tier (or to `tier` when
// given): a {"type": "route", "tier"} event comes first, and the final event adds `tier`, `can_escalate` (the
// question hit the step limit and a higher tier exists; only the user may accept) and `struggled` (the reason).
func StreamAgentEvents(ctx context.Context, a *Agent, task string, reset bool, tier string, emit func(Event) error) error {
	router := a.Router
	var route *Route
	if router != nil {
		if reset {
			a.RoutingState = &ConversationRouting{}
		}
		state := GetRoutingState(a)
		var chosen Route
		if tier != "" {
			chosen = router.RouteForTier(tier, "requested by the user")
		} else {
			chosen = router.Choose(ctx, task, state.Route, state.Question)
		}
		if err := ApplyRoute(a, chosen); err != nil {
			return err
		}
		route = &chosen
		state.Question, state.Route, state.CanEscalate = task, route, false
		if err := emit(Event{"type": "route", "tier": chosen.Tier}); err != nil {
			return err
		}
	}

	// Tool calls being streamed in the current step, keyed by their stream index
	calls := map[int]*streamingCall{}
	streamedAnswer := false

	return a.RunStream(ctx, task, reset, 0, func(event StreamEvent) error {
		switch e := event.(type) {
		case *models.ChatMessageStreamDelta:
			for _, delta := range e.ToolCalls {
				index := 0
				if delta.Index != nil {
					index = *delta.Index
				}
				call, ok := calls[index]
				if !ok {
					call = &streamingCall{}
					calls[index] = call
				}
				if delta.Function != nil {
					if delta.Function.Name != "" {
						call.name = delta.Function.Name
					}
					if s, ok := delta.Function.Arguments.(string); ok {
						call.args.WriteString(s)
					}
				}
				if call.name != FinalAnswerTool {
					continue
				}
				text, ok := PartialJSONStringValue(call.args.String(), "answer")
				if ok && len(text) > call.sent {
					chunk := controlRE.ReplaceAllString(text[call.sent:], "")
					call.sent = len(text)
					if chunk != "" {
						if err := emit(Event{"type": "delta", "text": chunk}); err != nil {
							return err
						}
						streamedAnswer = true
					}
				}
			}
		case *models.ChatMessageToolCall:
			if e.Function.Name != FinalAnswerTool {
				return emit(Event{"type": "tool", "name": e.Function.Name})
			}
		case *memory.ActionStep:
			if streamedAnswer && !e.IsFinalAnswer {
				if err := emit(Event{"type": "retract"}); err != nil {
					return err
				}
			}
			calls = map[int]*streamingCall{}
			streamedAnswer = false
		case *memory.FinalAnswerStep:
			final := Event{"type": "final", "answer": controlRE.ReplaceAllString(jsonval.Text(e.Output), "")}
			if route != nil {
				assessment := AssessTurn(a, final["answer"].(string), *route, router)
				final["tier"] = route.Tier
				final["can_escalate"] = assessment.CanEscalate
				GetRoutingState(a).CanEscalate = assessment.CanEscalate
				final["struggled"] = assessment.Struggled
			}
			return emit(final)
		}
		return nil
	})
}

// EscalateRefused is the answer to a retry request that wasn't offered.
const EscalateRefused = "This answer can't be retried with deeper reasoning: a retry is only offered when a question runs out of steps."

// EscalateEvents retries the conversation's last question one tier up, on the user's request, and only when
// that retry was offered (the question ran out of steps on a lower tier).
func EscalateEvents(ctx context.Context, a *Agent, emit func(Event) error) error {
	router := a.Router
	state := GetRoutingState(a)
	nextTier := ""
	if router != nil && state.Route != nil {
		nextTier = router.NextTier(state.Route.Tier)
	}
	if !state.CanEscalate || nextTier == "" || state.Question == "" {
		return emit(Event{"type": "final", "answer": EscalateRefused, "can_escalate": false})
	}
	state.CanEscalate = false // one retry per offer
	DropLastTurn(a)           // the retry replaces the struggling attempt instead of following it
	return StreamAgentEvents(ctx, a, state.Question, false, nextTier, emit)
}
