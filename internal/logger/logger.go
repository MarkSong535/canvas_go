// Package logger is the agent's logger: display blocks (panels, rules, markdown) always go to the console and the
// log file; plain log records go nowhere until InitLogger sets up handlers.
package logger

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

const YellowHex = "#d4b702"

type LogLevel int

const (
	Off   LogLevel = -1 // No output
	Error LogLevel = 0  // Only errors
	Info  LogLevel = 1  // Normal output (default)
	Debug LogLevel = 2  // Detailed output
)

const width = 100

// AgentLogger: Console receives display blocks, File mirrors them once initialized, and records go to a
// structured logger once initialized.
type AgentLogger struct {
	mu      sync.Mutex
	Level   LogLevel
	Console io.Writer
	File    io.Writer
	records *slog.Logger
}

// Default is the process-wide logger.
var Default = &AgentLogger{Level: Debug, Console: os.Stdout}

// InitLogger appends records to logPath; consoleOutput=false keeps the terminal free for the caller's own UI.
func (l *AgentLogger) InitLogger(logPath string, consoleOutput bool) error {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.File = f
	out := io.Writer(f)
	if consoleOutput {
		out = io.MultiWriter(f, os.Stderr)
		l.Console = os.Stdout
	} else {
		l.Console = io.Discard
	}
	l.records = slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return nil
}

// SetOutput sends display blocks to w (io.Discard silences them, e.g. in tests).
func (l *AgentLogger) SetOutput(w io.Writer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Console = w
}

func (l *AgentLogger) enabled(level LogLevel) bool { return level <= l.Level }

// block prints a pre-rendered block to the console and the log file.
func (l *AgentLogger) block(level LogLevel, text string) {
	if !l.enabled(level) {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Console != nil {
		io.WriteString(l.Console, text+"\n")
	}
	if l.File != nil {
		io.WriteString(l.File, text+"\n")
	}
}

// record writes a log record (dropped before InitLogger).
func (l *AgentLogger) record(level slog.Level, msg string) {
	l.mu.Lock()
	records := l.records
	l.mu.Unlock()
	if records != nil {
		records.Log(context.Background(), level, msg)
	}
}

// Log writes a message at the given level.
func (l *AgentLogger) Log(level LogLevel, msg string) {
	if l.enabled(level) {
		l.record(slog.LevelInfo, msg)
	}
}

func (l *AgentLogger) Infof(format string, args ...any) {
	l.record(slog.LevelInfo, fmt.Sprintf(format, args...))
}
func (l *AgentLogger) Warnf(format string, args ...any) {
	l.record(slog.LevelWarn, fmt.Sprintf(format, args...))
}
func (l *AgentLogger) Errorf(format string, args ...any) {
	l.record(slog.LevelError, fmt.Sprintf(format, args...))
}
func (l *AgentLogger) Debugf(format string, args ...any) {
	l.record(slog.LevelDebug, fmt.Sprintf(format, args...))
}

func (l *AgentLogger) LogError(msg string) {
	if l.enabled(Error) {
		l.record(slog.LevelError, msg)
	}
}

// Panel draws a titled box around text.
func Panel(title, body, subtitle string) string {
	inner := width - 4
	var b strings.Builder
	top := "╭─ " + title + " "
	b.WriteString(top + strings.Repeat("─", max(0, width-1-runeLen(top))) + "╮\n")
	for _, line := range wrap(body, inner) {
		b.WriteString("│ " + line + strings.Repeat(" ", max(0, inner-runeLen(line))) + " │\n")
	}
	bottom := "╰─"
	if subtitle != "" {
		bottom += " " + subtitle + " "
	}
	b.WriteString(bottom + strings.Repeat("─", max(0, width-1-runeLen(bottom))) + "╯")
	return b.String()
}

// Rule draws a horizontal rule with a title.
func Rule(title, char string) string {
	if title == "" {
		return strings.Repeat(char, width)
	}
	side := max(2, (width-runeLen(title)-2)/2)
	return strings.Repeat(char, side) + " " + title + " " + strings.Repeat(char, side)
}

func (l *AgentLogger) LogPanel(level LogLevel, title, body string) {
	l.block(level, Panel(title, body, ""))
}

func (l *AgentLogger) LogMarkdown(level LogLevel, title, content string) {
	if title != "" {
		l.block(level, Rule(title, "─")+"\n"+content)
		return
	}
	l.block(level, content)
}

func (l *AgentLogger) LogRule(level LogLevel, title string) {
	l.block(Info, Rule(title, "━"))
}

func (l *AgentLogger) LogTask(level LogLevel, content, subtitle, title string) {
	head := "New run"
	if title != "" {
		head += " - " + title
	}
	l.block(level, Panel(head, "\n"+content+"\n", subtitle))
}

func (l *AgentLogger) LogMessages(level LogLevel, messages []any) {
	parts := make([]string, len(messages))
	for i, m := range messages {
		out, _ := json.MarshalIndent(m, "", "    ")
		parts[i] = string(out)
	}
	l.block(level, strings.Join(parts, "\n"))
}

// LogText prints a plain block.
func (l *AgentLogger) LogText(level LogLevel, text string) { l.block(level, text) }

func runeLen(s string) int { return len([]rune(s)) }

func wrap(text string, w int) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		r := []rune(line)
		for len(r) > w {
			out = append(out, string(r[:w]))
			r = r[w:]
		}
		out = append(out, string(r))
	}
	return out
}

// TokenUsage is the token count for a step or run.
type TokenUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

func NewTokenUsage(input, output int) *TokenUsage {
	return &TokenUsage{InputTokens: input, OutputTokens: output, TotalTokens: input + output}
}

// Timing is the start/end time (seconds since the epoch) of a step or run.
type Timing struct {
	StartTime float64  `json:"start_time"`
	EndTime   *float64 `json:"end_time"`
}

func Now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Duration is nil until the step has ended.
func (t *Timing) Duration() *float64 {
	if t.EndTime == nil {
		return nil
	}
	d := *t.EndTime - t.StartTime
	return &d
}

func (t *Timing) End() {
	now := Now()
	t.EndTime = &now
}

// Monitor sums token usage and step durations across a run.
type Monitor struct {
	StepDurations         []float64
	TrackedModel          any
	Logger                *AgentLogger
	TotalInputTokenCount  int
	TotalOutputTokenCount int
}

func NewMonitor(model any, l *AgentLogger) *Monitor { return &Monitor{TrackedModel: model, Logger: l} }

func (m *Monitor) GetTotalTokenCounts() *TokenUsage {
	return NewTokenUsage(m.TotalInputTokenCount, m.TotalOutputTokenCount)
}

func (m *Monitor) Reset() {
	m.StepDurations = nil
	m.TotalInputTokenCount = 0
	m.TotalOutputTokenCount = 0
}

// UpdateMetrics records one finished step.
func (m *Monitor) UpdateMetrics(timing *Timing, usage *TokenUsage) {
	duration := 0.0
	if d := timing.Duration(); d != nil {
		duration = *d
	}
	m.StepDurations = append(m.StepDurations, duration)
	out := fmt.Sprintf("[Step %d: Duration %.2f seconds", len(m.StepDurations), duration)
	if usage != nil {
		m.TotalInputTokenCount += usage.InputTokens
		m.TotalOutputTokenCount += usage.OutputTokens
		out += fmt.Sprintf("| Input tokens: %s | Output tokens: %s", commas(m.TotalInputTokenCount), commas(m.TotalOutputTokenCount))
	}
	out += "]"
	m.Logger.LogText(Info, out)
}

func commas(n int) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
