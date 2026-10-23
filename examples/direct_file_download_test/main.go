// Command direct_file_download_test exercises the Canvas file tools directly (no agent): it lists courses and
// files, shows file details, reads file contents, searches files, and downloads files into examples/course.
//
// Needs CANVAS_URL and CANVAS_ACCESS_TOKEN (from the environment or .env). Run from the module root:
//
//	go run ./examples/direct_file_download_test                                   interactive mode
//	go run ./examples/direct_file_download_test <file_id>                         quick mode
//	go run ./examples/direct_file_download_test <file_id> --download             direct download
//	go run ./examples/direct_file_download_test <file_id> --download --course "Course Name"
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"canvas_go/internal/canvastools"
	"canvas_go/internal/env"
	"canvas_go/internal/jsonval"
	"canvas_go/internal/tools"
)

// ANSI styles
const (
	reset   = "\033[0m"
	bold    = "\033[1m"
	red     = "\033[31m"
	green   = "\033[32m"
	yellow  = "\033[33m"
	magenta = "\033[35m"
	cyan    = "\033[36m"
)

// DownloadFolder is where downloaded files are saved.
var DownloadFolder = filepath.Join("examples", "course")

var stdin = bufio.NewReader(os.Stdin)

// say prints one line in an ANSI style.
func say(style, text string) { fmt.Printf("%s%s%s\n", style, text, reset) }

func rule(style string) { say(style, strings.Repeat("=", 60)) }

// heading prints a titled section header.
func heading(title string) {
	say(cyan, "\n"+strings.Repeat("=", 60))
	say(bold+cyan, title)
	rule(cyan)
}

// input prints a prompt and reads one line (without the newline; "" at end of input).
func input(prompt string) string {
	fmt.Print(prompt)
	line, _ := stdin.ReadString('\n')
	return strings.TrimRight(line, "\r\n")
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

// showTool runs a tool and prints its output (green) or error (red); ok is false on error.
func showTool(ctx context.Context, t tools.Tool, args map[string]any) (tools.ToolResult, bool, error) {
	result, err := callTool(ctx, t, args)
	if err != nil {
		return result, false, err
	}
	if result.Error != "" {
		say(red, "❌ Error: "+result.Error)
		return result, false, nil
	}
	say(green, jsonval.Text(result.Output))
	return result, true, nil
}

// ensureDownloadFolder makes sure the download folder exists.
func ensureDownloadFolder() string {
	os.MkdirAll(DownloadFolder, 0o755)
	return DownloadFolder
}

func absolute(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

func printBanner() {
	say(bold+cyan, `
╔══════════════════════════════════════════════════════════════╗
║           Canvas Direct File Download Test                   ║
╚══════════════════════════════════════════════════════════════╝
    `)
	// Show the download folder path
	say(green, "📁 Download folder: "+absolute(ensureDownloadFolder()))
}

// testListCourses lists Canvas courses.
func testListCourses(ctx context.Context) error {
	heading("📚 Test 1: List all courses")
	_, _, err := showTool(ctx, canvastools.NewListCourses(), nil)
	return err
}

// testListFiles lists a course's files.
func testListFiles(ctx context.Context, courseID string) error {
	heading(fmt.Sprintf("📁 Test 2: List files for course %s", courseID))
	_, _, err := showTool(ctx, canvastools.NewGetFiles(), map[string]any{"course_id": courseID})
	return err
}

// testGetFileInfo fetches a file's metadata.
func testGetFileInfo(ctx context.Context, fileID string) error {
	heading(fmt.Sprintf("ℹ️  Test 3: Fetch metadata for file %s", fileID))
	_, _, err := showTool(ctx, canvastools.NewGetFileInfo(), map[string]any{"file_id": fileID})
	return err
}

// safeName keeps letters, digits, spaces, '-' and '_'.
func safeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// progressWriter prints a one-line download progress bar as bytes are written.
type progressWriter struct {
	total, done int64
	start, last time.Time
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if time.Since(p.last) >= 100*time.Millisecond {
		p.print()
	}
	return len(b), nil
}

func (p *progressWriter) print() {
	p.last = time.Now()
	const width = 30
	filled, percent := 0, 0.0
	if p.total > 0 {
		percent = float64(p.done) / float64(p.total) * 100
		filled = int(min(float64(width), float64(width)*float64(p.done)/float64(p.total)))
	}
	speed := float64(p.done) / max(time.Since(p.start).Seconds(), 1e-3) / (1024 * 1024)
	fmt.Printf("\r%sDownloading...%s %s%s %3.0f%% %.1f/%.1f MB %.1f MB/s", cyan, reset,
		strings.Repeat("━", filled), strings.Repeat("─", width-filled), percent,
		float64(p.done)/(1024*1024), float64(p.total)/(1024*1024), speed)
}

// downloadFileToDisk downloads a file from its Canvas "url" and saves it under the download folder.
func downloadFileToDisk(ctx context.Context, fileID, courseName string) string {
	heading("💾 Download file to local disk: " + fileID)

	path, err := func() (string, error) {
		// Query the Canvas API for the file's metadata and download URL
		result := canvastools.MakeRequest(ctx, "GET", "files/"+fileID, nil, nil)
		info, ok := result.(map[string]any)
		if !ok {
			return "", fmt.Errorf("unexpected file info response: %s", jsonval.JSON(result))
		}
		if msg, isErr := info["error"]; isErr {
			say(red, "❌ Error: "+jsonval.Text(msg))
			return "", nil
		}
		fileURL := jsonval.Text(info["url"])
		fileName := jsonval.Text(info["display_name"])
		if info["display_name"] == nil {
			fileName = "file_" + fileID
		}
		fileSize, _ := jsonval.Float(info["size"])

		// Create the target download directory
		downloadPath := DownloadFolder
		if courseName != "" {
			downloadPath = filepath.Join(DownloadFolder, safeName(courseName))
		}
		if err := os.MkdirAll(downloadPath, 0o755); err != nil {
			return "", err
		}

		// Compose the final file path
		filePath := filepath.Join(downloadPath, filepath.Base(fileName))

		say(yellow, "📄 File name: "+fileName)
		say(yellow, fmt.Sprintf("📦 Size: %.2f MB", fileSize/(1024*1024)))
		say(yellow, "💾 Saving to: "+absolute(filePath))

		// Stream the file to disk with a progress bar. No auth header: Canvas download URLs are pre-signed
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
		if err != nil {
			return "", err
		}
		resp, err := canvastools.HTTPClient.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			say(red, fmt.Sprintf("❌ Download failed (status: %d)", resp.StatusCode))
			return "", nil
		}
		f, err := os.Create(filePath)
		if err != nil {
			return "", err
		}
		progress := &progressWriter{total: int64(fileSize), start: time.Now()}
		_, err = io.CopyBuffer(io.MultiWriter(f, progress), resp.Body, make([]byte, 8192))
		progress.print()
		fmt.Println()
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return "", err
		}
		say(bold+green, "✅ File downloaded successfully!")
		say(green, "📍 Saved at: "+absolute(filePath))
		return filePath, nil
	}()
	if err != nil {
		say(red, fmt.Sprintf("❌ Download error: %v", err))
		return ""
	}
	return path
}

// testDownloadFile reads a file's content online, or shows its download link.
func testDownloadFile(ctx context.Context, fileID string, readContent bool) error {
	heading(fmt.Sprintf("📥 Test 4: Download file %s", fileID))
	say(cyan, "   Read content: "+jsonval.Text(readContent))
	rule(cyan)

	if readContent {
		_, _, err := showTool(ctx, canvastools.NewReadFile(), map[string]any{"file_id": fileID})
		return err
	}
	// Only the download link
	result := canvastools.MakeRequest(ctx, "GET", "files/"+fileID, nil, nil)
	info, ok := result.(map[string]any)
	if !ok {
		return fmt.Errorf("unexpected file info response: %s", jsonval.JSON(result))
	}
	if msg, isErr := info["error"]; isErr {
		say(red, "❌ Error: "+jsonval.Text(msg))
		return nil
	}
	say(green, fmt.Sprintf("📄 %s\n📎 Download link: %s", jsonval.Text(info["display_name"]), jsonval.Text(info["url"])))
	return nil
}

// testSearchFiles searches a course's files by keyword; ok is false when the search failed.
func testSearchFiles(ctx context.Context, courseID, searchTerm string) (bool, error) {
	heading(fmt.Sprintf("🔍 Test: Search keyword '%s'", searchTerm))
	_, ok, err := showTool(ctx, canvastools.NewSearchFiles(), map[string]any{"course_id": courseID, "search_term": searchTerm})
	return ok, err
}

const downloadOptions = "\nDownload options: \n" +
	"  1. View metadata only\n" +
	"  2. Download to local disk\n" +
	"  3. Read content online\n" +
	"Choose (1/2/3): "

const courseNamePrompt = "Enter course name (optional, used for folder name; leave blank to skip): "

// checkConfig prints the Canvas configuration; false when it is missing.
func checkConfig(details bool) bool {
	canvasURL, token := os.Getenv("CANVAS_URL"), os.Getenv("CANVAS_ACCESS_TOKEN")
	if canvasURL == "" || token == "" {
		say(bold+red, "\n❌ Error: Canvas configuration not found")
		if details {
			say(yellow, "Please ensure the .env file includes:")
			say(yellow, "  - CANVAS_URL")
			say(yellow, "  - CANVAS_ACCESS_TOKEN")
		}
		return false
	}
	say(green, "\n✓ Canvas URL: "+canvasURL)
	say(green, "✓ Access token configured")
	return true
}

// interactiveTest is the interactive test workflow.
func interactiveTest(ctx context.Context) {
	printBanner()
	if !checkConfig(true) {
		return
	}

	err := func() error {
		// 1. List courses
		if err := testListCourses(ctx); err != nil {
			return err
		}

		// 2. Select a course and list its files
		courseID := strings.TrimSpace(input("\nEnter course ID to view files (leave blank to skip): "))
		if courseID != "" {
			if err := testListFiles(ctx, courseID); err != nil {
				return err
			}

			// 3. Select a file for more details
			fileID := strings.TrimSpace(input("\nEnter file ID for detailed info (leave blank to skip): "))
			if fileID != "" {
				if err := testGetFileInfo(ctx, fileID); err != nil {
					return err
				}

				// 4. Decide how to handle the file
				switch input(downloadOptions) {
				case "2":
					downloadFileToDisk(ctx, fileID, strings.TrimSpace(input(courseNamePrompt)))
				case "3":
					if err := testDownloadFile(ctx, fileID, true); err != nil {
						return err
					}
				}
			}
		}

		// 5. Search files
		say(magenta, "\n"+strings.Repeat("=", 60))
		search := strings.TrimSpace(input("\nEnter a keyword to search files (leave blank to skip): "))
		if search != "" {
			// Canvas searches one course at a time: reuse the course from step 2, or ask for one
			searchCourse := courseID
			if searchCourse == "" {
				searchCourse = strings.TrimSpace(input("Enter the course ID to search in: "))
			}
			ok := false
			if searchCourse != "" {
				var err error
				if ok, err = testSearchFiles(ctx, searchCourse, search); err != nil {
					return err
				}
			}
			if ok {
				fileID := strings.TrimSpace(input("\nEnter the file ID to download (leave blank to skip): "))
				if fileID != "" {
					switch input(downloadOptions) {
					case "2":
						downloadFileToDisk(ctx, fileID, strings.TrimSpace(input(courseNamePrompt)))
					case "3":
						if err := testDownloadFile(ctx, fileID, true); err != nil {
							return err
						}
					default:
						if err := testGetFileInfo(ctx, fileID); err != nil {
							return err
						}
					}
				}
			}
		}

		// Completion message
		say(green, "\n"+strings.Repeat("=", 60))
		say(bold+green, "✅ All tests complete!")
		rule(green)
		return nil
	}()
	if err != nil {
		say(bold+red, fmt.Sprintf("\n❌ Error: %v", err))
	}
}

// quickTest is the quick test for a specific file ID.
func quickTest(ctx context.Context, fileID string, downloadToDisk bool, courseName string) {
	printBanner()
	say(bold+cyan, "\n🎯 Quick test - File ID: "+fileID)
	if !checkConfig(false) {
		return
	}

	err := func() error {
		// Fetch metadata for the file
		if err := testGetFileInfo(ctx, fileID); err != nil {
			return err
		}
		if downloadToDisk {
			say(yellow, "\n💾 Downloading file to local disk...")
			downloadFileToDisk(ctx, fileID, courseName)
		} else {
			say(yellow, "\n📎 Fetching download link...")
			if err := testDownloadFile(ctx, fileID, false); err != nil {
				return err
			}
			if strings.ToLower(input("\nDownload to local disk? (y/n): ")) == "y" {
				downloadFileToDisk(ctx, fileID, strings.TrimSpace(input(courseNamePrompt)))
			}
		}
		say(bold+green, "\n✅ Test complete!")
		return nil
	}()
	if err != nil {
		say(bold+red, fmt.Sprintf("\n❌ Error: %v", err))
	}
}

func printUsage() {
	say(bold+cyan, "\nUsage:")
	say(yellow, "  Interactive mode: go run ./examples/direct_file_download_test")
	say(yellow, "  Quick mode: go run ./examples/direct_file_download_test <file_id>")
	say(yellow, "  Direct download: go run ./examples/direct_file_download_test <file_id> --download")
	say(yellow, "  Categorized download: go run ./examples/direct_file_download_test <file_id> --download --course 'Course Name'")
	fmt.Println()
}

func main() {
	env.Load()
	ctx := context.Background()
	args := os.Args[1:]

	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		printUsage()
		return
	}
	if len(args) > 0 {
		// Quick-test mode
		fileID := args[0]
		downloadToDisk := slices.Contains(args, "--download") || slices.Contains(args, "-d")
		// Capture the course name if provided
		courseName := ""
		if i := slices.Index(args, "--course"); i >= 0 && i+1 < len(args) {
			courseName = args[i+1]
		}
		quickTest(ctx, fileID, downloadToDisk, courseName)
		return
	}
	// Interactive test mode
	printUsage()
	interactiveTest(ctx)
}
