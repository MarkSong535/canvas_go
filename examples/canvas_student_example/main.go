// Command canvas_student_example shows how to use the Canvas API toolset to help students manage coursework:
// it builds the Canvas Student Agent, offers a few sample tasks, and runs the one you pick (or your own prompt).
//
// Needs CANVAS_ACCESS_TOKEN, CANVAS_URL and OPENAI_API_KEY (from the environment or .env). Run from the module
// root:
//
//	go run ./examples/canvas_student_example
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"canvas_go/internal/config"
	"canvas_go/internal/env"
	"canvas_go/internal/jsonval"
	"canvas_go/internal/logger"
	"canvas_go/internal/models"
)

// ANSI styles
const (
	reset = "\033[0m"
	red   = "\033[31m"
)

// exampleTasks are the sample tasks offered by number.
var exampleTasks = []string{
	"List all of my courses",
	"Show my to-do items",
	"Get upcoming events",
	"Show the latest announcements",
	// Add more tasks if you like...
}

// isDigits reports whether s is a non-empty run of ASCII digits.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func run() error {
	ctx := context.Background()

	// 0. Validate required environment variables (the logger has no handlers yet, so print directly)
	if os.Getenv("CANVAS_ACCESS_TOKEN") == "" {
		fmt.Printf("%sMissing CANVAS_ACCESS_TOKEN environment variable%s\n", red, reset)
		fmt.Printf("%sSet CANVAS_ACCESS_TOKEN and CANVAS_URL in your .env file%s\n", red, reset)
		return nil
	}

	// 1. Initialize logger
	logDir := filepath.Join("workdir", "canvas_student")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	if err := logger.Default.InitLogger(filepath.Join(logDir, "log.txt"), true); err != nil {
		return err
	}

	// 2. Load the agent configuration
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// 3. Initialize the model manager
	if _, err := models.Manager.InitModels(false); err != nil {
		return err
	}

	// 4. Retrieve the model instance
	model, err := models.Manager.GetModel(cfg.ModelID)
	if err != nil {
		return err
	}

	// 5-6. Build the agent from the configuration
	a, err := config.BuildAgent(cfg, model)
	if err != nil {
		return err
	}

	logger.Default.Infof("Canvas Student Agent created successfully!")
	logger.Default.Infof("Loaded %d Canvas API tools", len(cfg.Tools))

	// 7. Prompt for the task to run
	fmt.Println("\n" + strings.Repeat("=", 60))
	fmt.Println("Canvas Student Agent Example")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println("\nAvailable sample tasks:")
	for i, task := range exampleTasks {
		fmt.Printf("%d. %s\n", i+1, task)
	}

	fmt.Println("\nEnter a task number or provide a custom prompt:")
	fmt.Print("> ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	userInput := strings.TrimSpace(line)

	// Pick the task to execute
	task := userInput
	if isDigits(userInput) {
		if n, err := strconv.Atoi(userInput); err == nil && n >= 1 && n <= len(exampleTasks) {
			task = exampleTasks[n-1]
		}
	}

	fmt.Printf("\nRunning task: %s\n", task)
	fmt.Println(strings.Repeat("-", 60))

	// 8. Run the agent
	result, err := a.Run(ctx, task, true)
	if err != nil {
		logger.Default.Errorf("Error while running the task: %v", err)
		fmt.Printf("\nError: %v\n\n", err)
		return nil
	}
	fmt.Println("\n" + strings.Repeat("=", 60))
	fmt.Println("Task completed!")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Printf("\nResult:\n%s\n\n", jsonval.Text(result))
	return nil
}

func main() {
	env.Load()
	if err := run(); err != nil {
		fmt.Printf("%sError: %v%s\n", red, err, reset)
		os.Exit(1)
	}
}
