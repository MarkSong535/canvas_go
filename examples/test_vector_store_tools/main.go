// Command test_vector_store_tools tests the vector store (course knowledge base) tools: list the vector
// stores, search one by calling the tool directly, or ask the Canvas Student Agent, which uses the tools itself.
//
// Needs OPENAI_API_KEY, CANVAS_URL and CANVAS_ACCESS_TOKEN (from the environment or .env). Run from the module
// root:
//
//	go run ./examples/test_vector_store_tools
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"canvas_go/internal/canvastools"
	"canvas_go/internal/config"
	"canvas_go/internal/env"
	"canvas_go/internal/jsonval"
	"canvas_go/internal/logger"
	"canvas_go/internal/models"
	"canvas_go/internal/tools"
)

// ANSI styles
const (
	reset   = "\033[0m"
	bold    = "\033[1m"
	dim     = "\033[2m"
	red     = "\033[31m"
	green   = "\033[32m"
	yellow  = "\033[33m"
	blue    = "\033[34m"
	magenta = "\033[35m"
	cyan    = "\033[36m"
)

var stdin = bufio.NewReader(os.Stdin)

// say prints one line in an ANSI style.
func say(style, text string) { fmt.Printf("%s%s%s\n", style, text, reset) }

// heading prints a titled section header.
func heading(style, title string) {
	say(style, "\n"+strings.Repeat("=", 60))
	say(bold+style, title)
	say(style, strings.Repeat("=", 60))
}

// input prints a prompt and reads one line (without the newline); ok is false at end of input.
func input(prompt string) (string, bool) {
	fmt.Print(prompt)
	line, err := stdin.ReadString('\n')
	if err != nil && line == "" {
		return "", false
	}
	return strings.TrimRight(line, "\r\n"), true
}

func goodbye() {
	say(cyan, "\n\n👋 Goodbye!")
	os.Exit(0)
}

// pick returns the n-th (1-based) choice when s is a number in range, else s itself.
func pick(s string, choices []string) string {
	if n, err := strconv.Atoi(s); err == nil && !strings.ContainsAny(s, "+-") && n >= 1 && n <= len(choices) {
		return choices[n-1]
	}
	return s
}

// callTool runs a tool and returns its ToolResult.
func callTool(ctx context.Context, t tools.Tool, args map[string]any) (tools.ToolResult, error) {
	out, err := tools.Call(ctx, t, args)
	if err != nil {
		return tools.ToolResult{}, err
	}
	result, _ := out.(tools.ToolResult)
	return result, nil
}

func printBanner() {
	say(bold+cyan, `
╔══════════════════════════════════════════════════════════════╗
║     Test Vector Store Tools Integration                     ║
║     Test Vector Store Tools Integration                     ║
╚══════════════════════════════════════════════════════════════╝
    `)
}

// testVectorStoreList lists the available vector stores.
func testVectorStoreList(ctx context.Context) (tools.ToolResult, error) {
	heading(cyan, "📋 Test 1: List Vector Stores")
	result, err := callTool(ctx, canvastools.NewVectorStoreList(), nil)
	if err != nil {
		return result, err
	}
	if result.Error != "" {
		say(red, "❌ Error: "+result.Error)
	} else {
		say(green, jsonval.Text(result.Output))
	}
	return result, nil
}

// testVectorStoreSearch searches a vector store (asking for one when vectorStoreID is empty).
func testVectorStoreSearch(ctx context.Context, vectorStoreID string) error {
	heading(cyan, "🔍 Test 2: Search a Vector Store")

	// If no vector store ID is provided, fetch the list first
	if vectorStoreID == "" {
		say(yellow, "\n⚠️  Vector Store ID not provided, retrieving list")
		list, err := testVectorStoreList(ctx)
		if err != nil {
			return err
		}
		if list.Error != "" || !jsonval.Truthy(list.Output) {
			say(red, "❌ Unable to retrieve the Vector Store list")
			return nil
		}
		// Prompt the user for the vector store ID
		vectorStoreID, _ = input("\nEnter a Vector Store ID: ")
	}

	// Example queries
	testQueries := []string{
		"What are the main topics covered in this course?",
		"Requirements for the first assignment",
		"Agent-Based Modeling",
	}

	say(green, "\n✓ Using vector store: "+vectorStoreID)
	say(bold+cyan, "\n📝 Sample queries:")
	for i, query := range testQueries {
		say(dim, fmt.Sprintf("  %d. %s", i+1, query))
	}

	// Let the user choose a query
	choice, _ := input("\nSelect a query (1-3) or enter a custom query: ")
	query := pick(choice, testQueries)

	say(cyan, fmt.Sprintf("\n🔍 Search query: \"%s\"", query))

	// Execute the search
	result, err := callTool(ctx, canvastools.NewVectorStoreSearch(),
		map[string]any{"vector_store_id": vectorStoreID, "query": query, "max_results": 3})
	if err != nil {
		return err
	}
	if result.Error != "" {
		say(red, "\n❌ Error: "+result.Error)
	} else {
		say(green, "\n"+jsonval.Text(result.Output))
	}
	return nil
}

// testWithAgent exercises the vector store tools through the agent.
func testWithAgent(ctx context.Context) {
	heading(magenta, "🤖 Test 3: Use tools through the agent")

	err := func() error {
		// Initialize the agent
		say(cyan, "\n🚀 Initializing Canvas Student Agent...")
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if _, err := models.Manager.InitModels(false); err != nil {
			return err
		}
		model, err := models.Manager.GetModel(cfg.ModelID)
		if err != nil {
			return err
		}
		a, err := config.BuildAgent(cfg, model)
		if err != nil {
			return err
		}
		say(green, "✓ Agent initialized")

		// Example prompts
		testQueries := []string{
			"List all available knowledge bases",
			"Search the knowledge base for Agent-Based Modeling",
			"What are the primary learning materials for this course?",
		}
		say(bold+cyan, "\n📝 Available sample prompts:")
		for i, query := range testQueries {
			say(dim, fmt.Sprintf("  %d. %s", i+1, query))
		}

		// Let the user choose or enter a prompt
		choice, _ := input("\nSelect a prompt (1-3) or enter a custom prompt (press q to exit): ")
		if strings.ToLower(choice) == "q" {
			return nil
		}
		query := pick(choice, testQueries)

		say(bold+blue, "\n💬 User: "+query)
		say(cyan, "\n🤖 Agent processing...\n")

		// Execute the request
		response, err := a.Run(ctx, query, true)
		if err != nil {
			return err
		}

		// Display the result
		say(green, logger.Panel("Agent Response", jsonval.Text(response), ""))
		return nil
	}()
	if err != nil {
		say(red, fmt.Sprintf("\n❌ Error: %v", err))
	}
}

func run(ctx context.Context) error {
	printBanner()

	// Check environment variables
	openaiKey := os.Getenv("OPENAI_API_KEY")
	canvasURL := os.Getenv("CANVAS_URL")
	canvasToken := os.Getenv("CANVAS_ACCESS_TOKEN")
	check := func(name, value string) {
		if value != "" {
			say(green, "  ✓ "+name)
		} else {
			say(red, "  ✗ "+name)
		}
	}
	say(bold+cyan, "\n📋 Environment check:")
	check("OPENAI_API_KEY", openaiKey)
	check("CANVAS_URL", canvasURL)
	check("CANVAS_ACCESS_TOKEN", canvasToken)

	if openaiKey == "" || canvasURL == "" || canvasToken == "" {
		say(bold+red, "\n❌ Missing required environment variables")
		say(yellow, "Please update the .env file with:")
		for _, missing := range [][2]string{{"OPENAI_API_KEY", openaiKey}, {"CANVAS_URL", canvasURL}, {"CANVAS_ACCESS_TOKEN", canvasToken}} {
			if missing[1] == "" {
				say(yellow, "  - "+missing[0])
			}
		}
		return nil
	}
	fmt.Println()

	// Main menu loop
	for {
		heading(cyan, "📋 Test menu")
		fmt.Println("  1. List all Vector Stores")
		fmt.Println("  2. Search a Vector Store (call tool directly)")
		fmt.Println("  3. Use the agent with tools (end-to-end)")
		fmt.Println("  q. Quit")

		choice, ok := input("\nChoose an option (1-3/q): ")
		if !ok {
			goodbye()
		}
		switch {
		case strings.ToLower(choice) == "q":
			say(cyan, "\n👋 Goodbye!")
			return nil
		case choice == "1":
			if _, err := testVectorStoreList(ctx); err != nil {
				return err
			}
		case choice == "2":
			if err := testVectorStoreSearch(ctx, ""); err != nil {
				return err
			}
		case choice == "3":
			testWithAgent(ctx)
		default:
			say(yellow, "⚠️  Invalid choice")
		}

		if _, ok := input("\nPress Enter to continue..."); !ok {
			goodbye()
		}
	}
}

func main() {
	env.Load()
	// Ctrl+C anywhere says goodbye
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	go func() {
		<-interrupts
		goodbye()
	}()
	if err := run(context.Background()); err != nil {
		say(bold+red, fmt.Sprintf("\n❌ Error: %v", err))
		os.Exit(1)
	}
}
