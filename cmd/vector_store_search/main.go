// Command vector_store_search is a manual, interactive test of OpenAI Vector Store search: it lists the vector
// stores (and the course mapping in file_index/vector_stores_mapping.json, when present), then lets you pick
// one and run search queries against it. Needs OPENAI_API_KEY (from the environment or .env).
//
//	go run ./cmd/vector_store_search                                          interactive mode
//	go run ./cmd/vector_store_search --vector-store-id vs_... --query "..."   quick search mode
//	go run ./cmd/vector_store_search --help
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/openai/openai-go/v3"

	"canvas_go/internal/downloader"
	"canvas_go/internal/env"
	"canvas_go/internal/jsonval"
	"canvas_go/internal/logger"
	"canvas_go/internal/models"
	"canvas_go/internal/textutil"
)

// ANSI styles
const (
	reset   = "\033[0m"
	bold    = "\033[1m"
	dim     = "\033[2m"
	red     = "\033[31m"
	green   = "\033[32m"
	yellow  = "\033[33m"
	magenta = "\033[35m"
	cyan    = "\033[36m"
)

// vectorStore is one entry of GET /vector_stores (only the fields shown here).
type vectorStore struct {
	ID         string  `json:"id"`
	Name       *string `json:"name"`
	FileCounts *struct {
		Total int `json:"total"`
	} `json:"file_counts"`
}

// searchResult is one entry of POST /vector_stores/{id}/search.
type searchResult struct {
	Score      *float64       `json:"score"`
	Filename   *string        `json:"filename"`
	Attributes map[string]any `json:"attributes"`
	Content    []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

var stdin = bufio.NewReader(os.Stdin)

// say prints one line in an ANSI style.
func say(style, text string) { fmt.Printf("%s%s%s\n", style, text, reset) }

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

func printBanner() {
	say(bold+cyan, `
╔══════════════════════════════════════════════════════════════╗
║           Vector Store Search Test                           ║
╚══════════════════════════════════════════════════════════════╝
    `)
}

// loadVectorStoreMapping loads the course -> vector store mapping written by file_index_downloader (nil if
// missing or unreadable).
func loadVectorStoreMapping() map[string]map[string]any {
	data, err := os.ReadFile(filepath.Join(downloader.DownloadRoot, "vector_stores_mapping.json"))
	if err != nil {
		return nil
	}
	var mapping map[string]map[string]any
	if json.Unmarshal(data, &mapping) != nil {
		return nil
	}
	return mapping
}

// listVectorStores lists all vector stores (nil on failure).
func listVectorStores(ctx context.Context, client openai.Client) []vectorStore {
	var response struct {
		Data []vectorStore `json:"data"`
	}
	if err := client.Get(ctx, "vector_stores?limit=100", nil, &response); err != nil {
		say(red, fmt.Sprintf("❌ Failed to fetch Vector Stores: %v", err))
		return nil
	}
	return response.Data
}

// searchVectorStore searches a vector store; ok is false when the search failed.
func searchVectorStore(ctx context.Context, client openai.Client, vectorStoreID, query string, maxResults int) ([]searchResult, bool) {
	var response struct {
		Data []searchResult `json:"data"`
	}
	payload := map[string]any{"query": query, "max_num_results": maxResults}
	path := "vector_stores/" + url.PathEscape(vectorStoreID) + "/search"
	if err := client.Post(ctx, path, payload, &response); err != nil {
		say(red, fmt.Sprintf("❌ Search failed: %v", err))
		return nil, false
	}
	return response.Data, true
}

// displaySearchResults prints each result in a panel.
func displaySearchResults(results []searchResult, query string) {
	if len(results) == 0 {
		say(yellow, "\n⚠️  No relevant results found")
		return
	}
	say(bold+green, fmt.Sprintf("\n📊 Search results (query: \"%s\")", query))
	say(green, fmt.Sprintf("Found %d relevant results\n", len(results)))

	for i, result := range results {
		var lines []string
		// Relevance score
		if result.Score != nil {
			lines = append(lines, fmt.Sprintf("📈 Relevance score: %.4f", *result.Score))
		}
		// File name
		if result.Filename != nil {
			lines = append(lines, "📄 File: "+*result.Filename)
		}
		// Metadata
		if len(result.Attributes) > 0 {
			lines = append(lines, "🏷️  Attributes: "+jsonval.JSON(result.Attributes))
		}
		// Content snippet
		if len(result.Content) > 0 {
			texts := make([]string, len(result.Content))
			for j, part := range result.Content {
				texts[j] = part.Text
			}
			content := strings.Join(texts, "\n")
			if textutil.Len(content) > 500 {
				content = textutil.Substr(content, 0, 500) + "..."
			}
			lines = append(lines, "\n📝 Content snippet:\n"+content)
		}
		say(cyan, logger.Panel(fmt.Sprintf("Result %d", i+1), strings.Join(lines, "\n"), ""))
		fmt.Println()
	}
}

// cell pads (or truncates with an ellipsis) text to width columns.
func cell(text string, width int, right bool) string {
	n := textutil.Len(text)
	switch {
	case n > width:
		return textutil.Substr(text, 0, width-1) + "…"
	case right:
		return strings.Repeat(" ", width-n) + text
	default:
		return text + strings.Repeat(" ", width-n)
	}
}

// printVectorStoreTable prints the available vector stores as an aligned table.
func printVectorStoreTable(stores []vectorStore) {
	rows := make([][4]string, len(stores))
	filesWidth := len("Files")
	for i, vs := range stores {
		name := "Unnamed"
		if vs.Name != nil && *vs.Name != "" {
			name = textutil.Substr(*vs.Name, 0, 40)
		}
		count := "N/A"
		if vs.FileCounts != nil {
			count = strconv.Itoa(vs.FileCounts.Total)
		}
		rows[i] = [4]string{strconv.Itoa(i + 1), vs.ID, name, count}
		filesWidth = max(filesWidth, len(count))
	}
	say(bold, "Available Vector Stores")
	fmt.Printf("%s%s  %s  %s  %s%s\n", bold, cell("#", 5, false), cell("ID", 30, false), cell("Name", 40, false),
		cell("Files", filesWidth, true), reset)
	fmt.Println(strings.Repeat("─", 5+30+40+filesWidth+6))
	for _, r := range rows {
		fmt.Printf("%s  %s  %s  %s\n", cyan+cell(r[0], 5, false)+reset, magenta+cell(r[1], 30, false)+reset,
			green+cell(r[2], 40, false)+reset, yellow+cell(r[3], filesWidth, true)+reset)
	}
}

// interactive is the interactive mode.
func interactive(ctx context.Context) {
	printBanner()

	// Check environment variables
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		say(bold+red, "❌ Error: OPENAI_API_KEY not found")
		say(yellow, "Please set OPENAI_API_KEY in your .env file")
		return
	}
	say(green, "✓ OpenAI API Key is configured\n")

	// Create the OpenAI client
	client := models.AssistantsClient(apiKey)
	say(green, "✓ OpenAI client created\n")

	// Load the mapping file
	mapping := loadVectorStoreMapping()

	// List all vector stores
	say(bold+cyan, "📋 Fetching Vector Stores list...\n")
	stores := listVectorStores(ctx, client)
	if len(stores) == 0 {
		say(yellow, "⚠️  No Vector Stores found")
		say(dim, "Run file_index_downloader --upload-only first to create Vector Stores")
		return
	}

	printVectorStoreTable(stores)
	fmt.Println()

	// If a mapping file exists, show the course mapping
	if len(mapping) > 0 {
		say(bold+cyan, "📚 Course mapping:")
		courses := make([]string, 0, len(mapping))
		for name := range mapping {
			courses = append(courses, name)
		}
		slices.Sort(courses)
		for _, name := range courses {
			id, ok := mapping[name]["vector_store_id"]
			if !ok {
				id = "N/A"
			}
			say(dim, fmt.Sprintf("  • %s: %s", name, jsonval.Text(id)))
		}
		fmt.Println()
	}

	// Interactive search loop
	for {
		// Select a vector store
		choice, ok := input("\nSelect a Vector Store number (or press q to quit): ")
		if !ok {
			goodbye()
		}
		if strings.ToLower(choice) == "q" {
			say(cyan, "\n👋 Goodbye!")
			return
		}
		n, err := strconv.Atoi(strings.TrimSpace(choice))
		if err != nil {
			say(yellow, "⚠️  Please enter a valid number")
			continue
		}
		if n < 1 || n > len(stores) {
			say(yellow, "⚠️  Invalid number")
			continue
		}
		selected := stores[n-1]
		name := "(unnamed)"
		if selected.Name != nil {
			name = *selected.Name
		}
		say(bold+green, "\n✓ Selected: "+name)
		say(dim, "  ID: "+selected.ID)

		// Enter search queries
		for {
			query, ok := input("\nEnter a search query (or press Enter to go back): ")
			if !ok {
				goodbye()
			}
			if strings.TrimSpace(query) == "" {
				break
			}

			// Ask for the number of results
			maxResultsInput, _ := input("Number of results (default 5): ")
			maxResults, err := strconv.Atoi(strings.TrimSpace(maxResultsInput))
			if err != nil {
				maxResults = 5
			}

			say(cyan, "\n🔍 Searching...")
			if results, ok := searchVectorStore(ctx, client, selected.ID, query, maxResults); ok {
				displaySearchResults(results, query)
			}

			// Ask whether to continue
			more, _ := input("\nContinue searching this Vector Store? (y/n): ")
			if strings.ToLower(more) != "y" {
				break
			}
		}
	}
}

// quickSearch is the command-line search mode.
func quickSearch(ctx context.Context, vectorStoreID, query string, maxResults int) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		say(bold+red, "❌ Error: OPENAI_API_KEY not found")
		return
	}
	client := models.AssistantsClient(apiKey)

	say(bold+cyan, "🔍 Search query: "+query)
	say(dim, fmt.Sprintf("   Vector Store: %s\n", vectorStoreID))

	if results, ok := searchVectorStore(ctx, client, vectorStoreID, query, maxResults); ok {
		displaySearchResults(results, query)
	}
}

func main() {
	env.Load()
	vectorStoreID := flag.String("vector-store-id", "", "Vector Store ID (quick search mode)")
	query := flag.String("query", "", "Search query (quick search mode)")
	maxResults := flag.Int("max-results", 5, "Maximum number of results")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Vector Store search test tool\n\nUsage of %s:\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	// Ctrl+C anywhere says goodbye
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	go func() {
		<-interrupts
		goodbye()
	}()

	ctx := context.Background()
	if *vectorStoreID != "" && *query != "" {
		// Quick search mode
		quickSearch(ctx, *vectorStoreID, *query, *maxResults)
	} else {
		// Interactive mode
		interactive(ctx)
	}
}
