package agent

/*
Per-question model routing.

Each question is routed ONCE, before the agent starts, to one of three tiers (config: Routing):

	simple    a single lookup of the student's Canvas data (due dates, grades, to-dos, course list)
	standard  explanations, or a few lookups combined
	hard      multi-step reasoning: derivations, proofs, problem solving, math or chemistry work

Obvious cases are decided by rules; anything unclear is classified by the cheap classifier model.
The model never changes in the middle of a question. If a question on a lower tier runs out of steps (MaxSteps)
without a final answer, the answer carries an offer to escalate, and only the user can accept it (retry the
question one tier up).
*/

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"canvas_go/internal/agenterr"
	"canvas_go/internal/memory"
	"canvas_go/internal/models"
)

var Tiers = []string{"simple", "standard", "hard"}

var (
	hardRE = regexp.MustCompile(`(?i)\b(derive|derivation|prove|proof|step[- ]by[- ]step|mechanism|solve|calculate|compute|optimi[sz]e|` +
		`explain why|why does|why do|compare and contrast|evaluate|analy[sz]e|design|work out|integral|derivative|` +
		`equation|balance)\b|\\frac|\\int|\\ce\{|[∫∑√≤≥≠]|\d\s*[\^*/]\s*\d`)
	lookupRE = regexp.MustCompile(`(?i)\b(due|deadlines?|to-?dos?|grades?|scores?|my courses|which courses|what courses|announcements?|calendar|` +
		`events?|upcoming|groups?|list|show|what'?s on|when is|when's|any new)\b`)
	explainRE  = regexp.MustCompile(`(?i)\b(explain|how does|how do|describe|summari[sz]e|help me understand|teach)\b`)
	followUpRE = regexp.MustCompile(`(?i)\b(it|that|this|those|these|them|the (first|second|third|last|other) one)\b`)
	wordRE     = regexp.MustCompile(`[a-z]+`)
)

const classifierPrompt = "You route questions for a Canvas study assistant to a model tier. Reply with exactly one word.\n" +
	"simple: a single lookup of the student's own Canvas data (due dates, grades, to-dos, course list, " +
	"announcements, calendar).\n" +
	"standard: explaining a concept, summarizing course material, or combining a few lookups.\n" +
	"hard: multi-step reasoning: derivations, proofs, solving problems, calculations, chemistry mechanisms, " +
	"or comparing many sources.\n" +
	"Question: %s\n" +
	"Previous question in this conversation (for context): %s\n" +
	"Tier:"

// Route is where one question was sent.
type Route struct {
	Tier    string
	ModelID string
	Reason  string
}

// Assessment is whether to offer the user a retry one tier up, and why.
type Assessment struct {
	Struggled   []string
	CanEscalate bool
}

// ConversationRouting is the last question and its route, kept per conversation for follow-ups and retries.
type ConversationRouting struct {
	Question string
	Route    *Route
	// CanEscalate is set only when the last question ran out of steps on a lower tier; a retry one tier up is
	// refused otherwise, whatever the client sends.
	CanEscalate bool
}

// RoutingConfig is the `routing` section of the agent config.
type RoutingConfig struct {
	Tiers             map[string]string
	ClassifierModelID string
}

// ModelRouter picks a model tier per question.
type ModelRouter struct {
	Tiers             map[string]string
	ClassifierModelID string
}

func NewModelRouter(tiers map[string]string, classifierModelID string) (*ModelRouter, error) {
	var missing []string
	for _, t := range Tiers {
		if _, ok := tiers[t]; !ok {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("routing config is missing tiers: %v", missing)
	}
	return &ModelRouter{Tiers: tiers, ClassifierModelID: classifierModelID}, nil
}

// BuildRouter makes the router from the config, or nil when routing is off.
func BuildRouter(cfg *RoutingConfig) (*ModelRouter, error) {
	if cfg == nil || len(cfg.Tiers) == 0 {
		return nil, nil
	}
	classifier := cfg.ClassifierModelID
	if classifier == "" {
		classifier = cfg.Tiers["simple"]
	}
	return NewModelRouter(cfg.Tiers, classifier)
}

// Choose routes a question: by rules when obvious, else by the classifier model.
func (r *ModelRouter) Choose(ctx context.Context, question string, previous *Route, previousQuestion string) Route {
	tier, reason := r.byRules(question, previous)
	if tier == "" {
		tier, reason = r.classify(ctx, question, previousQuestion)
	}
	return Route{Tier: tier, ModelID: r.Tiers[tier], Reason: reason}
}

// NextTier is the tier above, or "" at the top.
func (r *ModelRouter) NextTier(tier string) string {
	for i, t := range Tiers {
		if t == tier && i+1 < len(Tiers) {
			return Tiers[i+1]
		}
	}
	return ""
}

func (r *ModelRouter) RouteForTier(tier, reason string) Route {
	return Route{Tier: tier, ModelID: r.Tiers[tier], Reason: reason}
}

func (r *ModelRouter) byRules(question string, previous *Route) (string, string) {
	text := strings.TrimSpace(question)
	length := len([]rune(text))
	if hardRE.MatchString(text) || length > 400 || strings.Count(text, "?") > 2 {
		return "hard", "rule: reasoning, math or a long multi-part question"
	}
	if previous != nil && length < 60 && followUpRE.MatchString(text) {
		return previous.Tier, "rule: short follow-up keeps the previous tier"
	}
	if lookupRE.MatchString(text) && !explainRE.MatchString(text) && length < 120 {
		return "simple", "rule: a Canvas lookup"
	}
	return "", ""
}

func (r *ModelRouter) classify(ctx context.Context, question, previousQuestion string) (tier, reason string) {
	// Routing must never block an answer
	defer func() {
		if p := recover(); p != nil {
			tier, reason = "standard", "classifier failed (panic); default tier"
		}
	}()
	model, err := models.Manager.GetModel(r.ClassifierModelID)
	if err != nil {
		return "standard", fmt.Sprintf("classifier failed (%s); default tier", errorTypeName(err))
	}
	previous := truncate(previousQuestion, 300)
	if previous == "" {
		previous = "none"
	}
	prompt := fmt.Sprintf(classifierPrompt, truncate(question, 1500), previous)
	reply, err := model.Generate(ctx, []*models.ChatMessage{{Role: models.RoleUser, Content: models.Text(prompt)}},
		models.GenerateOptions{Extra: map[string]any{"reasoning_effort": "minimal"}})
	if err != nil {
		return "standard", fmt.Sprintf("classifier failed (%s); default tier", errorTypeName(err))
	}
	for _, word := range wordRE.FindAllString(strings.ToLower(reply.ContentText()), -1) {
		for _, t := range Tiers {
			if word == t {
				return t, "classifier"
			}
		}
	}
	return "standard", "classifier gave no tier; default tier"
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// GetRoutingState returns the agent's routing state, creating it if needed.
func GetRoutingState(a *Agent) *ConversationRouting {
	if a.RoutingState == nil {
		a.RoutingState = &ConversationRouting{}
	}
	return a.RoutingState
}

// ApplyRoute points the agent at the routed model for the next question (never changed mid-question).
func ApplyRoute(a *Agent, route Route) error {
	model, err := models.Manager.GetModel(route.ModelID)
	if err != nil {
		return err
	}
	a.Model = model
	a.Monitor.TrackedModel = model
	return nil
}

// lastTurnStart is the index of the current question's TaskStep in memory.
func lastTurnStart(a *Agent) int {
	for i := len(a.Memory.Steps) - 1; i >= 0; i-- {
		if _, ok := a.Memory.Steps[i].(*memory.TaskStep); ok {
			return i
		}
	}
	return 0
}

// DropLastTurn forgets the last question and its steps, so a retry starts from the conversation before it.
func DropLastTurn(a *Agent) {
	a.Memory.Steps = a.Memory.Steps[:lastTurnStart(a)]
}

// AssessTurn decides whether to offer the user a retry one tier up: only when the question used up every step
// (MaxSteps) without the model giving a final answer.
func AssessTurn(a *Agent, answer string, route Route, router *ModelRouter) Assessment {
	reasons := []string{}
	for _, s := range a.Memory.Steps[lastTurnStart(a):] {
		if step, ok := s.(*memory.ActionStep); ok && agenterr.IsType(step.Error, agenterr.TypeAgentMaxStepsError) {
			reasons = append(reasons, "hit the step limit")
			break
		}
	}
	return Assessment{Struggled: reasons, CanEscalate: len(reasons) > 0 && router.NextTier(route.Tier) != ""}
}
