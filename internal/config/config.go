// Package config is the Canvas Student Agent configuration: an agent tailored for student access to Canvas
// LMS with the complete Canvas API toolset available to student accounts.
package config

import (
	"os"

	"canvas_go/internal/agent"
	"canvas_go/internal/canvastools"
	"canvas_go/internal/models"
	"canvas_go/internal/tools"
)

// AgentConfig describes the agent to build.
type AgentConfig struct {
	Type            string
	Name            string
	Description     string
	ModelID         string // must be one of the models registered in internal/models (all <= $5 / 1M tokens)
	MaxSteps        int
	StreamOutputs   bool // stream model output so the CLI / web UI can render the answer as it is generated
	MaxHistoryTurns int  // multi-turn memory: earlier exchanges kept in context per conversation
	// Each question is routed once, before the agent starts, to a tier by difficulty (internal/agent/routing.go).
	// If a question runs out of steps (MaxSteps) on a lower tier, the user is offered a retry one tier up;
	// nothing escalates on its own.
	Routing      *agent.RoutingConfig
	TemplatePath string // "" = the built-in prompts (internal/agent/prompts/general_agent.yaml)
	Tools        []tools.Tool
}

// CanvasTools creates every Canvas tool available to students (plus knowledge-base and LaTeX tools).
func CanvasTools() ([]tools.Tool, error) {
	if os.Getenv("CANVAS_ACCESS_TOKEN") == "" {
		return nil, canvastools.ErrNoToken
	}
	return []tools.Tool{
		canvastools.NewListCourses(),          // List enrolled courses
		canvastools.NewGetAssignments(),       // Retrieve assignments
		canvastools.NewSubmitAssignment(),     // Submit an assignment (never reaches live Canvas: blocked, or recorded in sandbox)
		canvastools.NewGetModules(),           // Retrieve course modules
		canvastools.NewGetModuleItems(),       // Retrieve module items
		canvastools.NewGetFiles(),             // List course files
		canvastools.NewGetFileInfo(),          // Retrieve file details
		canvastools.NewReadFile(),             // Read a file's text (sandbox: from the downloaded copy)
		canvastools.NewGetFolders(),           // List folders
		canvastools.NewGetFolderFiles(),       // List files inside a folder
		canvastools.NewSearchFiles(),          // Search files by keyword
		canvastools.NewGetDiscussions(),       // Retrieve discussions
		canvastools.NewPostDiscussion(),       // Post a discussion reply (never reaches live Canvas: blocked, or recorded in sandbox)
		canvastools.NewGetAnnouncements(),     // Retrieve announcements
		canvastools.NewGetCalendarEvents(),    // Retrieve calendar events
		canvastools.NewGetGrades(),            // Retrieve grades
		canvastools.NewGetPages(),             // List course pages
		canvastools.NewGetPageContent(),       // Retrieve page content
		canvastools.NewGetQuizzes(),           // List quizzes
		canvastools.NewGetTodoItems(),         // Retrieve to-do items
		canvastools.NewGetUpcomingEvents(),    // Retrieve upcoming events
		canvastools.NewGetGroups(),            // List groups
		canvastools.NewVectorStoreList(),      // List knowledge bases
		canvastools.NewVectorStoreSearch(),    // Search knowledge bases
		canvastools.NewVectorStoreListFiles(), // List files inside a knowledge base
		canvastools.NewVectorStoreGetFile(),   // Read the contents of a file
		canvastools.NewCheckLatex(),           // Compile math / chemfig the way the student will see it
	}, nil
}

// Load returns the Canvas student agent configuration.
func Load() (*AgentConfig, error) {
	toolset, err := CanvasTools()
	if err != nil {
		return nil, err
	}
	return &AgentConfig{
		Type:            "general_agent",
		Name:            "canvas_student_agent",
		Description:     "Canvas LMS study assistant that helps manage courses, assignments, discussions, and more",
		ModelID:         "gpt-6-luna",
		MaxSteps:        15,
		StreamOutputs:   true,
		MaxHistoryTurns: 10,
		Routing: &agent.RoutingConfig{
			Tiers:             map[string]string{"simple": "gpt-5-nano", "standard": "gpt-6-luna", "hard": "gpt-5.4-mini"},
			ClassifierModelID: "gpt-5-nano",
		},
		Tools: toolset,
	}, nil
}

// BuildAgent builds the agent from a config with the given model, through the agent type registry.
func BuildAgent(cfg *AgentConfig, model models.Model) (*agent.Agent, error) {
	turns := cfg.MaxHistoryTurns
	return agent.Build(cfg.Type, agent.Options{
		Tools:           cfg.Tools,
		Model:           model,
		TemplatePath:    cfg.TemplatePath,
		MaxSteps:        cfg.MaxSteps,
		MaxHistoryTurns: &turns,
		StreamOutputs:   cfg.StreamOutputs,
		Name:            cfg.Name,
		Description:     cfg.Description,
	})
}

// BuildWithRouter builds the agent with the configured model and attaches the router.
func BuildWithRouter(cfg *AgentConfig) (*agent.Agent, error) {
	model, err := models.Manager.GetModel(cfg.ModelID)
	if err != nil {
		return nil, err
	}
	a, err := BuildAgent(cfg, model)
	if err != nil {
		return nil, err
	}
	if a.Router, err = agent.BuildRouter(cfg.Routing); err != nil {
		return nil, err
	}
	return a, nil
}
