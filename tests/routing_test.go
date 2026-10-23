package tests

// Model routing tests: rules, the classifier, one route per question, and escalation that only happens when
// the user asks for it. Uses the simulated streaming OpenAI client.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"canvas_go/internal/agent"
	"canvas_go/internal/memory"
	"canvas_go/internal/models"
	"canvas_go/internal/wsserver"
)

// SimulatedClassifier stands in for the classifier model: replies with a fixed word, or fails.
type SimulatedClassifier struct {
	mu    sync.Mutex
	Word  string // "" fails
	Calls int
}

func (c *SimulatedClassifier) ModelID() string { return "simulated-classifier" }

func (c *SimulatedClassifier) Generate(ctx context.Context, messages []*models.ChatMessage, opts models.GenerateOptions) (*models.ChatMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Calls++
	if c.Word == "" {
		return nil, errors.New("classifier down")
	}
	return &models.ChatMessage{Role: models.RoleAssistant, Content: c.Word}, nil
}

// makeRouter returns a router over three simulated tier models, their scripted clients, and the classifier.
func makeRouter(classifierWord string) (*agent.ModelRouter, map[string]*SimulatedCompletions, *SimulatedClassifier) {
	completions := map[string]*SimulatedCompletions{}
	tiers := map[string]string{}
	for _, tier := range []string{"simple", "standard", "hard"} {
		completions[tier] = &SimulatedCompletions{}
		models.Manager.Register("simulated-"+tier, models.NewOpenAIServerModel("gpt-6-luna", completions[tier], nil))
		tiers[tier] = "simulated-" + tier
	}
	classifier := &SimulatedClassifier{Word: classifierWord}
	models.Manager.Register("simulated-classifier", classifier)
	router, err := agent.NewModelRouter(tiers, "simulated-classifier")
	if err != nil {
		panic(err)
	}
	return router, completions, classifier
}

func collectEvents(run func(emit func(agent.Event) error) error) []agent.Event {
	var events []agent.Event
	if err := run(func(e agent.Event) error { events = append(events, e); return nil }); err != nil {
		panic(err)
	}
	return events
}

func streamEvents(a *agent.Agent, question string) []agent.Event {
	return collectEvents(func(emit func(agent.Event) error) error {
		return agent.StreamAgentEvents(context.Background(), a, question, false, "", emit)
	})
}

func escalate(a *agent.Agent) []agent.Event {
	return collectEvents(func(emit func(agent.Event) error) error {
		return agent.EscalateEvents(context.Background(), a, emit)
	})
}

func TestRulesRouteObviousQuestionsWithoutTheClassifier(t *testing.T) {
	router, _, classifier := makeRouter("standard")
	choose := func(q string, previous *agent.Route) string {
		return router.Choose(context.Background(), q, previous, "").Tier
	}
	cases := map[string]string{
		"What's due this week?":                  "simple",
		"Show my grades in Chemistry":            "simple",
		"What is due tomorrow?":                  "simple",
		"Derive why A* is optimal, step by step": "hard",
		"Balance Fe + O2 -> Fe2O3":               "hard",
		"What is \\frac{1}{2} + 3^2?":            "hard",
	}
	for question, want := range cases {
		if got := choose(question, nil); got != want {
			t.Fatalf("%q -> %s, want %s", question, got, want)
		}
	}
	if got := choose("what about the second one?", &agent.Route{Tier: "hard", ModelID: "simulated-hard"}); got != "hard" {
		t.Fatalf("follow-ups keep the tier, got %s", got)
	}
	if classifier.Calls != 0 {
		t.Fatalf("classifier called %d times", classifier.Calls)
	}
}

func TestUnclearQuestionsGoToTheClassifier(t *testing.T) {
	router, _, classifier := makeRouter("standard")
	route := router.Choose(context.Background(), "What is a heuristic in my AI course?", nil, "")
	if route.Tier != "standard" || route.Reason != "classifier" || classifier.Calls != 1 {
		t.Fatalf("route %+v, %d calls", route, classifier.Calls)
	}
	router, _, _ = makeRouter("") // classifier failure falls back to the standard tier
	route = router.Choose(context.Background(), "What is a heuristic in my AI course?", nil, "")
	if route.Tier != "standard" || !strings.Contains(route.Reason, "failed") {
		t.Fatalf("route %+v", route)
	}
}

func TestRouteOnceOfferEscalationAndRetryOnlyOnRequest(t *testing.T) {
	router, completions, _ := makeRouter("standard")
	a, _ := makeAgent(nil, nil)
	a.Router = router

	// A lookup goes to the simple tier and finishes cleanly: no offer
	completions["simple"].Add(toolTurn("a1"), answerTurn("a2", `{"answer": "Lab 1 is due Friday."}`))
	events := streamEvents(a, "What's due this week?")
	if !reflect.DeepEqual(events[0], agent.Event{"type": "route", "tier": "simple"}) {
		t.Fatalf("first event: %v", events[0])
	}
	last := events[len(events)-1]
	if last["tier"] != "simple" || last["can_escalate"] != false {
		t.Fatalf("final: %v", last)
	}

	// A single repeat (routine; answered from memory) with a good answer is not a struggle
	completions["simple"].Add(toolTurn("g1"), toolTurn("g2"), answerTurn("g3", `{"answer": "Nothing is due."}`))
	last = lastOf(streamEvents(a, "Any to-dos?"))
	if last["can_escalate"] != false || len(last["struggled"].([]string)) != 0 {
		t.Fatalf("final: %v", last)
	}

	// Going in circles or saying "I couldn't" is not enough: the offer only comes at the step limit
	completions["simple"].Add(toolTurn("b1"), toolTurn("b2"), toolTurn("b4"), answerTurn("b3", `{"answer": "I couldn't find any."}`))
	last = lastOf(streamEvents(a, "Any new announcements?"))
	if last["can_escalate"] != false || len(last["struggled"].([]string)) != 0 {
		t.Fatalf("final: %v", last)
	}

	// The simple tier runs out of steps: an offer, but nothing escalates on its own
	completions["simple"].Add(stepLimitTurns("s")...)
	completions["simple"].FinalText = "Not sure."
	last = lastOf(streamEvents(a, "Show my grades"))
	if last["can_escalate"] != true || !reflect.DeepEqual(last["struggled"], []string{"hit the step limit"}) {
		t.Fatalf("final: %v", last)
	}
	if len(completions["standard"].Calls) != 0 {
		t.Fatal("no automatic escalation")
	}

	// The user accepts: the same question is retried one tier up, replacing the struggling attempt
	completions["standard"].Add(answerTurn("c1", `{"answer": "Your current grade is 93.5%."}`))
	events = escalate(a)
	if !reflect.DeepEqual(events[0], agent.Event{"type": "route", "tier": "standard"}) {
		t.Fatalf("first event: %v", events[0])
	}
	if lastOf(events)["answer"] != "Your current grade is 93.5%." {
		t.Fatalf("final: %v", lastOf(events))
	}
	var tasks []string
	for _, s := range a.Memory.Steps {
		if task, ok := s.(*memory.TaskStep); ok {
			tasks = append(tasks, task.Task)
		}
	}
	if len(tasks) != 4 || !strings.Contains(tasks[3], "Show my grades") {
		t.Fatalf("the struggling attempt was replaced: %d tasks", len(tasks))
	}
	sent := mustJSON(completions["standard"].LastMessages())
	if strings.Contains(sent, "Not sure.") {
		t.Fatal("the stronger model doesn't see the failed attempt")
	}
	if !strings.Contains(sent, "Lab 1 is due Friday.") {
		t.Fatal("but it keeps the earlier conversation")
	}

	// One retry per offer: asking again is refused without calling any model
	calls := len(completions["hard"].Calls)
	if last := lastOf(escalate(a)); last["answer"] != agent.EscalateRefused || len(completions["hard"].Calls) != calls {
		t.Fatalf("a second retry must be refused: %v", last)
	}
}

func TestNoOfferAtTheTopTier(t *testing.T) {
	router, completions, _ := makeRouter("standard")
	a, _ := makeAgent(nil, nil)
	a.Router = router
	completions["hard"].Add(stepLimitTurns("d")...)
	completions["hard"].FinalText = "I cannot tell."
	last := lastOf(streamEvents(a, "Prove the theorem step by step"))
	if last["tier"] != "hard" || len(last["struggled"].([]string)) == 0 || last["can_escalate"] != false {
		t.Fatalf("final: %v", last)
	}
	last = lastOf(escalate(a))
	if last["can_escalate"] != false || last["answer"] != agent.EscalateRefused {
		t.Fatalf("final: %v", last)
	}
}

func TestWebSocketEscalateMessage(t *testing.T) {
	router, completions, _ := makeRouter("standard")
	a, _ := makeAgent(nil, nil)
	a.Router = router
	wsserver.SetAgent(a)
	defer wsserver.SetAgent(nil)
	completions["simple"].Add(stepLimitTurns("e")...)
	completions["simple"].FinalText = "Hmm."
	completions["standard"].Add(answerTurn("f1", `{"answer": "Here it is."}`))
	var frames []wsserver.Frame
	send := func(f wsserver.Frame) error { frames = append(frames, f); return nil }
	ctx := context.Background()
	first, _, err := wsserver.HandleMessage(ctx, wsserver.Frame{"type": "chat", "query": "List my courses"}, nil, send)
	if err != nil {
		t.Fatal(err)
	}
	retry, _, err := wsserver.HandleMessage(ctx, wsserver.Frame{"type": "escalate"}, nil, send)
	if err != nil {
		t.Fatal(err)
	}
	if first["tier"] != "simple" || first["can_escalate"] != true {
		t.Fatalf("first: %v", first)
	}
	if retry["tier"] != "standard" || retry["answer"] != "Here it is." || retry["can_escalate"] != false {
		t.Fatalf("retry: %v", retry)
	}
	found := false
	for _, f := range frames {
		if reflect.DeepEqual(f, wsserver.Frame{"type": "route", "tier": "standard"}) {
			found = true
		}
	}
	if !found {
		t.Fatal("the retry announces its route")
	}
}

func lastOf(events []agent.Event) agent.Event { return events[len(events)-1] }

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func TestEscalationRefusedWithoutAnOffer(t *testing.T) {
	router, completions, _ := makeRouter("standard")
	a, _ := makeAgent(nil, nil)
	a.Router = router
	completions["simple"].Add(answerTurn("n1", `{"answer": "Lab 1 is due Friday."}`))
	if last := lastOf(streamEvents(a, "What's due this week?")); last["can_escalate"] != false {
		t.Fatalf("final: %v", last)
	}
	steps := len(a.Memory.Steps)
	// The client asks anyway (e.g. a crafted message): refused, nothing retried, the conversation untouched
	last := lastOf(escalate(a))
	if last["answer"] != agent.EscalateRefused || last["can_escalate"] != false {
		t.Fatalf("final: %v", last)
	}
	if len(completions["standard"].Calls) != 0 || len(a.Memory.Steps) != steps {
		t.Fatal("a refused retry must not call a model or change memory")
	}
	// Same through the WebSocket handler
	wsserver.SetAgent(a)
	defer wsserver.SetAgent(nil)
	reply, _, err := wsserver.HandleMessage(context.Background(), wsserver.Frame{"type": "escalate"}, nil, func(wsserver.Frame) error { return nil })
	if err != nil || reply["answer"] != agent.EscalateRefused || reply["can_escalate"] != false {
		t.Fatalf("reply %v, err %v", reply, err)
	}
}
