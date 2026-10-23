// Package chat is the Canvas Student Agent interactive console: a command-line chat with the agent in natural
// language, with answers streaming into the terminal as they are generated.
package chat

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"canvas_go/internal/agent"
	"canvas_go/internal/config"
	"canvas_go/internal/jsonval"
	"canvas_go/internal/logger"
	"canvas_go/internal/models"
	"canvas_go/internal/sandbox"
)

// ANSI styles
const (
	reset   = "\033[0m"
	bold    = "\033[1m"
	dim     = "\033[2m"
	italic  = "\033[3m"
	red     = "\033[31m"
	green   = "\033[32m"
	yellow  = "\033[33m"
	magenta = "\033[35m"
	cyan    = "\033[36m"
)

// Console is where the chat writes (os.Stdout normally).
type Console struct {
	Out io.Writer
	In  *bufio.Reader
}

func NewConsole() *Console { return &Console{Out: os.Stdout, In: bufio.NewReader(os.Stdin)} }

func (c *Console) Printf(format string, args ...any) { fmt.Fprintf(c.Out, format, args...) }

func (c *Console) PrintBanner() {
	c.Printf("%s%s%s\n", bold+cyan, `
    ╔═══════════════════════════════════════════════════════════╗
    ║                                                           ║
    ║        🎓 Canvas Student Agent Interactive Console 🎓    ║
    ║                                                           ║
    ║           Your Canvas LMS study companion                ║
    ║                                                           ║
    ╚═══════════════════════════════════════════════════════════╝
    `, reset)
}

func (c *Console) panel(title, color, body string) {
	c.Out.Write([]byte(color + logger.Panel(title, body, "") + reset + "\n"))
}

func (c *Console) PrintHelp() {
	c.panel("Help", green, `📖 Available commands:

help     - Show this guide
new      - Start a new conversation (forget earlier messages)
deeper   - Retry the last question with a stronger model
clear    - Clear the terminal window
examples - Show sample prompts
status   - Check Canvas connectivity
exit     - Quit the program
quit     - Quit the program

💬 How to use:

Type a question or instruction and the agent will handle it. For example:
- "List all of my courses"
- "Show my to-do items"
- "Get the assignments for Calculus"
- "What events are happening today?"
- "Show the latest announcements"`)
}

func (c *Console) PrintExamples() {
	c.panel("Sample Prompts", cyan, `📝 Sample prompts:

📚 Courses:
- List every course I am enrolled in
- Show the details for course 123
- Outline the module structure for Calculus

📝 Assignments:
- Show all assignments on my to-do list
- Get the assignments for course 123
- Submit assignment 456 with URL: https://example.com

📅 Schedule:
- What is on my calendar today?
- Show this week's calendar events
- List upcoming events

💬 Discussions:
- Show the discussions for course 456
- Reply to discussion 123: I agree with this point

📊 Grades:
- Show my grades in course 789
- Display all of my grades

📢 Announcements:
- Show the latest announcements
- Get announcements across all courses

📄 Files and resources:
- List every file in course 456
- Search the course for PDF files
- Show the course page list`)
}

// CheckCanvasConnection checks whether the Canvas API can be reached.
func CheckCanvasConnection(ctx context.Context) (bool, string) {
	canvasURL := os.Getenv("CANVAS_URL")
	if canvasURL == "" {
		canvasURL = "https://canvas.instructure.com"
	}
	token := os.Getenv("CANVAS_ACCESS_TOKEN")
	if token == "" {
		return false, "Missing CANVAS_ACCESS_TOKEN environment variable"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, canvasURL+"/api/v1/users/self", nil)
	if err != nil {
		return false, "Connection error: " + err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, "Connection error: " + err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false, fmt.Sprintf("Connection failed (status code %d)", resp.StatusCode)
	}
	data, _ := io.ReadAll(resp.Body)
	user, _ := jsonval.Decode(data)
	name := "Unknown"
	if m, ok := user.(map[string]any); ok && m["name"] != nil {
		name = jsonval.Text(m["name"])
	}
	return true, "Connected as " + name
}

func (c *Console) PrintStatus(ctx context.Context) {
	c.Printf("\n%s🔍 Checking system status...%s\n", bold+cyan, reset)
	canvasURL := os.Getenv("CANVAS_URL")
	if canvasURL == "" {
		canvasURL = "Not configured"
	}
	configured := func(key string) string {
		if os.Getenv(key) != "" {
			return "Configured ✓"
		}
		return "Missing ✗"
	}
	modelsText := strings.Join(models.Manager.ListModels(), ", ")
	if modelsText == "" {
		modelsText = "Not initialized"
	}
	connected, message := CheckCanvasConnection(ctx)
	status := "✗ " + message
	if connected {
		status = "✓ " + message
	}
	c.panel("System Status", cyan, fmt.Sprintf(`📊 System status:

Canvas URL: %s
Canvas Token: %s
OpenAI API Key: %s
Registered Models: %s
Canvas Connectivity: %s`, canvasURL, configured("CANVAS_ACCESS_TOKEN"), configured("OPENAI_API_KEY"), modelsText, status))
}

// InitializeAgent builds the Canvas Student Agent (nil when it can't).
func (c *Console) InitializeAgent() *agent.Agent {
	c.Printf("\n%s🚀 Initializing the Canvas Student Agent...%s\n", bold+cyan, reset)
	if os.Getenv("CANVAS_ACCESS_TOKEN") == "" {
		c.Printf("%sError: CANVAS_ACCESS_TOKEN is not configured%s\n", bold+red, reset)
		c.Printf("%sSet CANVAS_ACCESS_TOKEN and CANVAS_URL in your .env file.%s\n", yellow, reset)
		c.Printf("\n%sHow to generate a Canvas access token:%s\n", cyan, reset)
		c.Printf("1. Sign in to your Canvas account\n2. Open the 'Account' menu on the left\n3. Choose 'Settings'\n" +
			"4. Scroll to 'Approved Integrations'\n5. Click '+ New Access Token'\n6. Describe the purpose and generate the token\n" +
			"7. Copy the token and add it to your .env file\n\n")
		return nil
	}
	// Quiet logger: agent internals go to the log file, the terminal shows the chat UI
	logDir := filepath.Join("workdir", "canvas_chat")
	os.MkdirAll(logDir, 0o755)
	if err := logger.Default.InitLogger(filepath.Join(logDir, "log.txt"), false); err != nil {
		c.Printf("%s✗ Failed to initialize the agent: %v%s\n", bold+red, err, reset)
		return nil
	}
	cfg, err := config.Load()
	if err != nil {
		c.Printf("%s✗ Failed to initialize the agent: %v%s\n", bold+red, err, reset)
		return nil
	}
	if _, err := models.Manager.InitModels(false); err != nil {
		c.Printf("%s✗ Failed to initialize the agent: %v%s\n", bold+red, err, reset)
		return nil
	}
	available := models.Manager.ListModels()
	c.Printf("%sRegistered %d models: %s%s\n", green, len(available), orNone(available), reset)
	if _, err := models.Manager.GetModel(cfg.ModelID); err != nil {
		c.Printf("%sError: model '%s' is not registered%s\n", bold+red, cfg.ModelID, reset)
		c.Printf("%sAvailable models: %s%s\n", yellow, orNone(available), reset)
		c.Printf("%sUpdate ModelID in internal/config/config.go or verify your environment variables.%s\n", cyan, reset)
		return nil
	}
	a, err := config.BuildWithRouter(cfg)
	if err != nil {
		c.Printf("%s✗ Failed to initialize the agent: %v%s\n", bold+red, err, reset)
		return nil
	}
	c.Printf("%s✓ Agent initialized successfully!%s\n", bold+green, reset)
	c.Printf("%sLoaded %d Canvas API tools%s\n\n", green, len(cfg.Tools), reset)
	return a
}

func orNone(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}

// EventSource produces the UI events of one answer.
type EventSource func(emit func(agent.Event) error) error

// ProcessQuery handles one question, printing the answer as it streams in. events replaces the normal run,
// e.g. EscalateEvents when the user asks for a deeper retry.
func (c *Console) ProcessQuery(ctx context.Context, a *agent.Agent, query string, events EventSource) {
	if events == nil {
		events = func(emit func(agent.Event) error) error {
			// reset=false keeps earlier exchanges in the agent's memory (multi-turn conversation)
			return agent.StreamAgentEvents(ctx, a, query, false, "", emit)
		}
	}
	var streamed strings.Builder
	var final agent.Event
	tier := ""
	c.Printf("\n%s💡 Answer%s\n%sThinking…%s\n", bold+green, reset, cyan, reset)
	err := events(func(event agent.Event) error {
		switch event["type"] {
		case "route":
			tier = jsonval.Text(event["tier"])
			c.Printf("%s(tier: %s)%s\n", dim, tier, reset)
		case "tool":
			c.Printf("%s🔧 Calling %s…%s\n", dim, jsonval.Text(event["name"]), reset)
		case "delta":
			text := jsonval.Text(event["text"])
			streamed.WriteString(text)
			c.Printf("%s", text)
		case "retract":
			streamed.Reset()
			c.Printf("\n%s(Retrying…)%s\n", yellow, reset)
		case "final":
			final = event
		}
		return nil
	})
	if err != nil {
		c.Printf("\n%s✗ Error while processing the query: %v%s\n\n", bold+red, err, reset)
		return
	}
	answer := jsonval.Text(final["answer"])
	if streamed.String() != answer {
		if streamed.Len() > 0 {
			c.Printf("\n%s──%s\n", dim, reset)
		}
		c.Printf("%s", RenderMarkdown(answer))
	}
	c.Printf("\n")
	// Never escalate on our own: offer it, and let the user decide
	if jsonval.Truthy(final["can_escalate"]) {
		var reasons []string
		if list, ok := final["struggled"].([]string); ok {
			reasons = list
		}
		c.Printf("%sThis one looked hard (%s). Type %sdeeper%s%s to retry it with a stronger model.%s\n",
			yellow, strings.Join(reasons, ", "), bold, reset, yellow, reset)
	}
}

var (
	boldRE    = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	headingRE = regexp.MustCompile(`(?m)^(#{1,6}) +(.+)$`)
)

// RenderMarkdown lightly styles Markdown for the terminal (headings and bold).
func RenderMarkdown(text string) string {
	text = headingRE.ReplaceAllString(text, bold+cyan+"$2"+reset)
	return boldRE.ReplaceAllString(text, bold+"$1"+reset)
}

func (c *Console) readLine(prompt string) (string, error) {
	c.Printf("%s", prompt)
	line, err := c.In.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// Main is the entry point of the interactive console.
func Main() int {
	c := NewConsole()
	ctx := context.Background()
	c.PrintBanner()
	c.Printf("\n%sStarting up...%s\n", cyan, reset)

	if _, err := sandbox.Start(ctx, func(m string) { c.Printf("%s%s%s\n", magenta, m, reset) }); err != nil {
		c.Printf("%sCanvas sandbox failed to start: %v%s\n", bold+red, err, reset)
		return 1
	}
	a := c.InitializeAgent()
	if a == nil {
		c.Printf("%sUnable to start the agent; exiting.%s\n", bold+red, reset)
		return 1
	}
	c.PrintHelp()
	c.Printf("\n%s✨ Ready! Start chatting whenever you are.%s\n", bold+green, reset)
	c.Printf("%sTip: type 'help' for guidance or 'exit' to quit.%s\n\n", dim, reset)

	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)

	conversations := 0
	for {
		line, err := c.readLine(fmt.Sprintf("\n%sYou%s: ", bold+cyan, reset))
		if err != nil {
			break
		}
		if line == "" {
			continue
		}
		switch strings.ToLower(line) {
		case "exit", "quit", "q":
			c.Printf("\n%s👋 Goodbye! Happy studying!%s\n\n", bold+cyan, reset)
			goto done
		case "help":
			c.PrintHelp()
			continue
		case "examples":
			c.PrintExamples()
			continue
		case "status":
			c.PrintStatus(ctx)
			continue
		case "clear":
			clearScreen()
			c.PrintBanner()
			continue
		case "new":
			a.Memory.Reset()
			a.RoutingState = &agent.ConversationRouting{}
			conversations = 0
			c.Printf("%s✓ Started a new conversation.%s\n", green, reset)
			continue
		case "deeper":
			c.runInterruptible(ctx, interrupts, a, func(qctx context.Context) {
				c.ProcessQuery(qctx, a, "", func(emit func(agent.Event) error) error { return agent.EscalateEvents(qctx, a, emit) })
			})
			continue
		}
		conversations++
		if quit := c.runInterruptible(ctx, interrupts, a, func(qctx context.Context) { c.ProcessQuery(qctx, a, line, nil) }); quit {
			break
		}
	}
done:
	if conversations > 0 {
		c.Printf("\n%sThis session included %d exchanges.%s\n", dim, conversations, reset)
	}
	return 0
}

// runInterruptible runs one answer; Ctrl+C cancels it and asks whether to exit. Returns true to quit.
func (c *Console) runInterruptible(ctx context.Context, interrupts chan os.Signal, a *agent.Agent, run func(context.Context)) bool {
	qctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		run(qctx)
	}()
	select {
	case <-done:
		return false
	case <-interrupts:
		cancel()
		<-done
		c.Printf("\n\n%sDetected Ctrl+C%s\n", yellow, reset)
		answer, _ := c.readLine(cyan + "Do you want to exit? [y/n] (n): " + reset)
		if strings.ToLower(answer) == "y" {
			c.Printf("\n%s👋 Goodbye! Happy studying!%s\n\n", bold+cyan, reset)
			os.Exit(0)
		}
		return false
	}
}

func clearScreen() {
	cmd := exec.Command("clear")
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "cls")
	}
	cmd.Stdout = os.Stdout
	cmd.Run()
}
