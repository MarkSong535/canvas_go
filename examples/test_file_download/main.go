// Command test_file_download shows how to use the Canvas Student Agent to work with files: list courses,
// list a course's files, download (read) a file, and search files, each as a natural-language request.
//
// Needs CANVAS_URL, CANVAS_ACCESS_TOKEN and OPENAI_API_KEY (from the environment or .env). Run from the module
// root:
//
//	go run ./examples/test_file_download             interactive mode
//	go run ./examples/test_file_download <file_id>   quick download test
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"canvas_go/internal/agent"
	"canvas_go/internal/config"
	"canvas_go/internal/env"
	"canvas_go/internal/jsonval"
	"canvas_go/internal/models"
)

// ANSI styles
const (
	reset  = "\033[0m"
	bold   = "\033[1m"
	red    = "\033[31m"
	green  = "\033[32m"
	yellow = "\033[33m"
	cyan   = "\033[36m"
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

// input prints a prompt and reads one line (without the newline; "" at end of input).
func input(prompt string) string {
	fmt.Print(prompt)
	line, _ := stdin.ReadString('\n')
	return strings.TrimRight(line, "\r\n")
}

func printBanner() {
	say(bold+cyan, `
╔══════════════════════════════════════════════════════════════╗
║           Canvas File Download Test                          ║
║           File Download Test Example                         ║
╚══════════════════════════════════════════════════════════════╝
    `)
}

// newAgent builds the Canvas Student Agent from the default configuration.
func newAgent() (*agent.Agent, error) {
	say(cyan, "\nInitializing Canvas Agent...")
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if _, err := models.Manager.InitModels(false); err != nil {
		return nil, err
	}
	model, err := models.Manager.GetModel(cfg.ModelID)
	if err != nil {
		return nil, err
	}
	a, err := config.BuildAgent(cfg, model)
	if err != nil {
		return nil, err
	}
	say(bold+green, "✓ Agent initialized")
	return a, nil
}

// ask runs one query through the agent and prints the result under a label.
func ask(ctx context.Context, a *agent.Agent, query, label string) error {
	say(yellow, "\n💬 Query: "+query)
	result, err := a.Run(ctx, query, true)
	if err != nil {
		return err
	}
	say(bold+green, "\n"+label)
	fmt.Println(jsonval.Text(result))
	return nil
}

// testFileDownload runs the interactive file download workflow.
func testFileDownload(ctx context.Context) {
	printBanner()

	// Validate required environment variables
	canvasURL, token := os.Getenv("CANVAS_URL"), os.Getenv("CANVAS_ACCESS_TOKEN")
	if canvasURL == "" || token == "" {
		say(bold+red, "\n❌ Error: Canvas configuration not found")
		say(yellow, "Please ensure the .env file includes:")
		say(yellow, "  - CANVAS_URL")
		say(yellow, "  - CANVAS_ACCESS_TOKEN")
		return
	}
	say(green, "\n✓ Canvas URL: "+canvasURL)
	say(green, "✓ Access token configured")

	err := func() error {
		a, err := newAgent()
		if err != nil {
			return err
		}

		// Test 1: List courses
		heading(cyan, "Test 1: List courses")
		if err := ask(ctx, a, "List all of my courses", "📋 Result:"); err != nil {
			return err
		}

		// Test 2: List files
		heading(cyan, "Test 2: List files")
		// Allow the user to choose a course
		say(yellow, "\nPlease review the course list above.")
		courseID := input("\nEnter a course ID to list files (leave blank to skip): ")
		if strings.TrimSpace(courseID) != "" {
			if err := ask(ctx, a, fmt.Sprintf("List all files for course %s", courseID), "📁 File list:"); err != nil {
				return err
			}

			// Test 3: Download a file
			heading(cyan, "Test 3: Download a file")
			fileID := input("\nEnter the file ID to download (leave blank to skip): ")
			if strings.TrimSpace(fileID) != "" {
				if err := ask(ctx, a, fmt.Sprintf("Download file %s", fileID), "📥 Download result:"); err != nil {
					return err
				}
			}
		} else {
			say(yellow, "\n⏩ File operations skipped")
		}

		// Test 4: Search files
		heading(cyan, "Test 4: Search files")
		searchTerm := input("\nEnter a search keyword (leave blank to skip): ")
		if strings.TrimSpace(searchTerm) != "" {
			query := fmt.Sprintf("Search for files whose names include '%s'", searchTerm)
			if err := ask(ctx, a, query, "🔍 Search results:"); err != nil {
				return err
			}

			// Optionally download one of the matches
			choice := input("\nDownload one of the search results? Enter the file ID (leave blank to skip): ")
			if strings.TrimSpace(choice) != "" {
				if err := ask(ctx, a, fmt.Sprintf("Download file %s", choice), "📥 Download result:"); err != nil {
					return err
				}
			}
		} else {
			say(yellow, "\n⏩ Search test skipped")
		}

		// Complete
		heading(green, "✅ All tests complete!")
		return nil
	}()
	if err != nil {
		say(bold+red, fmt.Sprintf("\n❌ Error: %v", err))
	}
}

// quickDownloadTest downloads a specified file ID through the agent.
func quickDownloadTest(ctx context.Context, fileID string) {
	printBanner()
	say(bold+cyan, "\n🎯 Quick download test - File ID: "+fileID)

	err := func() error {
		a, err := newAgent()
		if err != nil {
			return err
		}
		heading(cyan, "Begin file download")
		if err := ask(ctx, a, fmt.Sprintf("Download file %s", fileID), "📥 Download result:"); err != nil {
			return err
		}
		heading(green, "✅ Test complete!")
		return nil
	}()
	if err != nil {
		say(bold+red, fmt.Sprintf("\n❌ Error: %v", err))
	}
}

func main() {
	env.Load()
	ctx := context.Background()
	if len(os.Args) > 1 {
		if os.Args[1] == "-h" || os.Args[1] == "--help" {
			fmt.Println("Usage:\n  go run ./examples/test_file_download             interactive mode\n" +
				"  go run ./examples/test_file_download <file_id>   quick download test")
			return
		}
		// Quick download mode
		quickDownloadTest(ctx, os.Args[1])
		return
	}
	// Interactive test mode
	testFileDownload(ctx)
}
