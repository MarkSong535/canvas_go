// Command canvas_sandbox manages the Canvas sandbox:
//
//	canvas_sandbox sync     capture everything from Canvas now (refreshes an existing sandbox)
//	canvas_sandbox status   show what the sandbox holds
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"canvas_go/internal/env"
	"canvas_go/internal/sandbox"
)

const usage = `canvas_sandbox sync     capture everything from Canvas now (refreshes an existing sandbox)
canvas_sandbox status   show what the sandbox holds`

func countLines(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for scanner.Scan() {
		n++
	}
	return n
}

func main() {
	env.Load()
	command := "status"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	store := sandbox.NewStore(sandbox.Dir())
	switch command {
	case "sync":
		// The capture step: the only one that uses the real Canvas URL and access token
		url, token := os.Getenv("CANVAS_URL"), os.Getenv("CANVAS_ACCESS_TOKEN")
		if url == "" || token == "" {
			fmt.Println("CANVAS_URL and CANVAS_ACCESS_TOKEN must be set to the real Canvas values.")
			os.Exit(1)
		}
		if _, err := sandbox.Sync(context.Background(), store, url, token, func(m string) { fmt.Println(m) }); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
	case "status":
		if !store.IsComplete() {
			fmt.Printf("No complete sandbox at %s. Run: canvas_sandbox sync\n", store.Root)
			os.Exit(1)
		}
		if _, err := store.Load(); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		errorsCount := 0
		if errs, ok := store.Manifest["errors"].(map[string]any); ok {
			errorsCount = len(errs)
		}
		out, _ := json.MarshalIndent(map[string]any{
			"dir":                   store.Root,
			"captured":              store.Manifest["created"],
			"source":                store.Manifest["source"],
			"counts":                store.Manifest["counts"],
			"endpoints_with_errors": errorsCount,
			"writes_recorded":       countLines(filepath.Join(store.Root, "writes.jsonl")),
			"requests_served":       countLines(filepath.Join(store.Root, "requests.jsonl")),
		}, "", "  ")
		fmt.Println(string(out))
	default:
		fmt.Println(usage)
		os.Exit(1)
	}
}
