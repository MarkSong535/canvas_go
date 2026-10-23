// Package wsserver is the WebSocket server that bridges web clients to the Canvas agent.
package wsserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"canvas_go/internal/agent"
	"canvas_go/internal/config"
	"canvas_go/internal/downloader"
	"canvas_go/internal/env"
	"canvas_go/internal/jsonval"
	"canvas_go/internal/latexsvg"
	"canvas_go/internal/models"
	"canvas_go/internal/sandbox"
)

// Frame is one JSON message.
type Frame = map[string]any

// SendFunc sends a frame to the client.
type SendFunc func(Frame) error

// AllowedOrigins may open a WebSocket (other origins are refused at the handshake). WS_ALLOWED_ORIGINS
// (comma-separated) replaces the list, e.g. to let a local dev page talk to a staging deployment.
var AllowedOrigins = []string{"https://markso.ng"}

// TurnstileVerifyURL is Cloudflare's token redemption endpoint. TURNSTILE_VERIFY_URL overrides it, e.g. to
// point a staging deployment at a stand-in verifier (cmd/ws_smoke -simulated-turnstile) for scripted smoke tests.
var TurnstileVerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

var (
	agentMu    sync.Mutex
	agentCache *agent.Agent
)

// SetAgent replaces the cached agent (tests use a scripted one; nil rebuilds it on next use).
func SetAgent(a *agent.Agent) {
	agentMu.Lock()
	defer agentMu.Unlock()
	agentCache = a
}

// BuildAgent creates and caches the Canvas agent.
func BuildAgent() (*agent.Agent, error) {
	agentMu.Lock()
	defer agentMu.Unlock()
	if agentCache != nil {
		return agentCache, nil
	}
	var missing []string
	for _, name := range []string{"CANVAS_ACCESS_TOKEN", "CANVAS_URL"} {
		if os.Getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("Missing environment variables: %s", strings.Join(missing, ", "))
	}
	if _, err := models.Manager.InitModels(false); err != nil {
		return nil, err
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if _, err := models.Manager.GetModel(cfg.ModelID); err != nil {
		available := strings.Join(models.Manager.ListModels(), ", ")
		if available == "" {
			available = "none"
		}
		return nil, fmt.Errorf("Model '%s' is not registered. Available: %s", cfg.ModelID, available)
	}
	a, err := config.BuildWithRouter(cfg)
	if err != nil {
		return nil, err
	}
	agentCache = a
	return a, nil
}

// HandleAgentQuery runs a chat payload through the agent, streaming progress through send.
//
// Streams {"type": "route"}, {"type": "tool"}, {"type": "delta"} and {"type": "retract"} frames while the agent
// works; the returned {"type": "answer", ...} frame is the authoritative final reply.
func HandleAgentQuery(ctx context.Context, message Frame, send SendFunc) (Frame, error) {
	query, ok := message["query"].(string)
	if !ok || strings.TrimSpace(query) == "" {
		return Frame{"error": "Payload must include a non-empty 'query' field."}, nil
	}
	a, err := BuildAgent()
	if err != nil {
		return nil, err
	}
	// reset=false: the agent keeps this connection's earlier exchanges (memory is cleared per connection)
	return streamAnswer(ctx, send, func(emit func(agent.Event) error) error {
		return agent.StreamAgentEvents(ctx, a, query, false, "", emit)
	})
}

// HandleEscalation retries the last question one tier up (the user accepted the offer).
func HandleEscalation(ctx context.Context, send SendFunc) (Frame, error) {
	a, err := BuildAgent()
	if err != nil {
		return nil, err
	}
	return streamAnswer(ctx, send, func(emit func(agent.Event) error) error {
		return agent.EscalateEvents(ctx, a, emit)
	})
}

func streamAnswer(ctx context.Context, send SendFunc, run func(func(agent.Event) error) error) (Frame, error) {
	final := agent.Event{"answer": ""}
	err := run(func(event agent.Event) error {
		if event["type"] == "final" {
			final = event
			return nil
		}
		return send(event)
	})
	if err != nil {
		return nil, err
	}
	answer := jsonval.Text(final["answer"])
	frame := Frame{"type": "answer", "answer": answer,
		// Math and ```chemfig blocks are rendered to SVG here; the page swaps each snippet for its SVG by key
		"renders": latexsvg.RenderAnswer(ctx, answer)}
	if tier, ok := final["tier"]; ok {
		frame["tier"] = tier
	}
	// Only the user decides whether a struggling answer is retried with a stronger model ({"type": "escalate"})
	frame["can_escalate"] = jsonval.Truthy(final["can_escalate"])
	return frame, nil
}

// ResetConversation forgets earlier exchanges so the next chat message starts a new conversation.
func ResetConversation() error {
	a, err := BuildAgent()
	if err != nil {
		return err
	}
	a.Memory.Reset()
	a.RoutingState = &agent.ConversationRouting{}
	return nil
}

// FetchCourseCatalog returns the student's active courses.
func FetchCourseCatalog(ctx context.Context) ([]map[string]any, error) {
	canvasURL, token := os.Getenv("CANVAS_URL"), os.Getenv("CANVAS_ACCESS_TOKEN")
	if canvasURL == "" || token == "" {
		return nil, errors.New("Canvas configuration missing.")
	}
	headers := map[string]string{"Authorization": "Bearer " + token, "Accept": "application/json"}
	return downloader.FetchAllPages(ctx, canvasURL+"/api/v1/courses", headers,
		[]downloader.Param{{Key: "enrollment_state", Value: "active"}, {Key: "per_page", Value: "100"}}, true)
}

// toInt reads an integer from a JSON value (a number, or a numeric string).
func toInt(v any) (int64, bool) {
	switch x := v.(type) {
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n, err == nil
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	if f, ok := jsonval.Float(v); ok {
		return int64(f), true
	}
	return 0, false
}

func intList(raw any, field string) ([]int64, Frame) {
	items, ok := raw.([]any)
	if !ok {
		return nil, Frame{"error": field + " must be a list of integers."}
	}
	out := make([]int64, 0, len(items))
	for _, item := range items {
		n, ok := toInt(item)
		if !ok {
			return nil, Frame{"error": field + " must contain valid integers."}
		}
		out = append(out, n)
	}
	return out, nil
}

// HandleDownloadRequest runs the file index downloader with automated selections.
func HandleDownloadRequest(ctx context.Context, message Frame, pending []map[string]any) (Frame, []map[string]any) {
	skipDownload := jsonval.Truthy(message["skip_download"])
	courseIDsRaw, hasIDs := message["course_ids"]
	courseIndicesRaw, hasIndices := message["course_indices"]
	hasIDs = hasIDs && courseIDsRaw != nil
	hasIndices = hasIndices && courseIndicesRaw != nil
	autoConfirm, hasConfirm := message["auto_confirm"]
	if !hasConfirm {
		autoConfirm = true
	}

	var courseIDs []int64
	if hasIDs {
		ids, problem := intList(courseIDsRaw, "course_ids")
		if problem != nil {
			return problem, pending
		}
		courseIDs = ids
	}
	if hasIndices {
		if _, ok := courseIndicesRaw.([]any); !ok {
			return Frame{"error": "course_indices must be a list of integers."}, pending
		}
		if pending == nil {
			return Frame{"error": "Course list not initialized. Request the course list first."}, pending
		}
		indices, problem := intList(courseIndicesRaw, "course_indices")
		if problem != nil {
			return problem, pending
		}
		if len(indices) == 0 {
			return Frame{"error": "course_indices cannot be empty."}, pending
		}
		var invalid []int64
		for _, i := range indices {
			if i < 1 || i > int64(len(pending)) {
				invalid = append(invalid, i)
			}
		}
		if len(invalid) > 0 {
			return Frame{"error": fmt.Sprintf("course_indices out of range: %s", formatInts(invalid))}, pending
		}
		seen := map[int64]bool{}
		courseIDs = nil
		for _, i := range indices {
			if !seen[i] {
				seen[i] = true
				id, _ := toInt(pending[i-1]["id"])
				courseIDs = append(courseIDs, id)
			}
		}
	}
	confirm, ok := autoConfirm.(bool)
	if !ok {
		return Frame{"error": "auto_confirm must be a boolean."}, pending
	}

	if !hasIDs && !hasIndices && !skipDownload {
		courses, err := FetchCourseCatalog(ctx)
		if err != nil {
			return Frame{"status": "course_list_error", "error": fmt.Sprintf("Failed to fetch courses: %v", err)}, pending
		}
		list := make([]any, len(courses))
		for i, c := range courses {
			list[i] = map[string]any{"index": i + 1, "id": c["id"], "name": c["name"], "code": c["course_code"]}
		}
		return Frame{"status": "course_list", "courses": list}, courses
	}
	if len(courseIDs) == 0 && !skipDownload {
		return Frame{"error": "Provide course_ids or course_indices after requesting the course list."}, pending
	}

	downloader.ResetStats()
	downloader.ConfigureAutomation(courseIDs, confirm)
	func() {
		defer downloader.ClearAutomation()
		downloader.Main(ctx, skipDownload)
	}()
	return Frame{"status": "completed", "stats": downloader.CurrentStats()}, nil
}

func formatInts(items []int64) string {
	parts := make([]string, len(items))
	for i, n := range items {
		parts[i] = strconv.FormatInt(n, 10)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// HandleMessage routes a message to its handler by type.
func HandleMessage(ctx context.Context, message Frame, pending []map[string]any, send SendFunc) (Frame, []map[string]any, error) {
	messageType, _ := message["type"].(string)
	if !jsonval.Truthy(message["type"]) {
		messageType = "chat"
	}
	switch messageType {
	case "chat", "query":
		response, err := HandleAgentQuery(ctx, message, send)
		return response, pending, err
	case "reset":
		if err := ResetConversation(); err != nil {
			return nil, pending, err
		}
		return Frame{"status": "reset"}, pending, nil
	case "escalate":
		response, err := HandleEscalation(ctx, send)
		return response, pending, err
	case "download":
		response, courses := HandleDownloadRequest(ctx, message, pending)
		return response, courses, nil
	}
	return Frame{"error": fmt.Sprintf("Unsupported message type: %s", jsonval.Text(message["type"]))}, pending, nil
}

// VerifyTurnstileToken redeems a single-use challenge token; every validation failure denies access.
func VerifyTurnstileToken(ctx context.Context, token any) bool {
	secret := os.Getenv("TURNSTILE_SECRET")
	hostnames := map[string]bool{}
	for _, h := range strings.Split(os.Getenv("TURNSTILE_HOSTNAMES"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			hostnames[h] = true
		}
	}
	action := env.Get("TURNSTILE_ACTION", "canvas_ws")
	text, ok := token.(string)
	if secret == "" || len(hostnames) == 0 || !ok || strings.TrimSpace(text) == "" || len([]rune(text)) > 2048 {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	form := url.Values{"secret": {secret}, "response": {text}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, env.Get("TURNSTILE_VERIFY_URL", TurnstileVerifyURL), strings.NewReader(form.Encode()))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false
	}
	var result map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return false
	}
	hostname, isString := result["hostname"].(string)
	return result["success"] == true && isString && hostnames[hostname] && result["action"] == action
}

// ParseSessionDuration parses SESSION_DURATION (minutes): default 30, minimum 1.
func ParseSessionDuration(raw string, set bool) int {
	if !set {
		return 30
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 30
	}
	return max(n, 1)
}

// Server accepts one verified WebSocket connection at a time.
type Server struct {
	SessionMinutes int
	mu             sync.Mutex
	active         *websocket.Conn
}

func NewServer() *Server {
	raw, set := os.LookupEnv("SESSION_DURATION")
	return &Server{SessionMinutes: ParseSessionDuration(raw, set)}
}

func originAllowed(origin string) bool {
	allowed := AllowedOrigins
	if raw := os.Getenv("WS_ALLOWED_ORIGINS"); strings.TrimSpace(raw) != "" {
		allowed = nil
		for _, o := range strings.Split(raw, ",") {
			if o = strings.TrimSpace(o); o != "" {
				allowed = append(allowed, o)
			}
		}
	}
	for _, o := range allowed {
		if origin == o {
			return true
		}
	}
	return false
}

func sendJSON(ctx context.Context, conn *websocket.Conn, frame Frame) error {
	return conn.Write(ctx, websocket.MessageText, []byte(jsonval.JSON(frame)))
}

// authenticate requires verification before acquiring a session or dispatching any request.
func authenticate(ctx context.Context, conn *websocket.Conn) bool {
	verified := func() bool {
		readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		_, raw, err := conn.Read(readCtx)
		if err != nil || len(raw) > 4096 {
			return false
		}
		var payload map[string]any
		if json.Unmarshal(raw, &payload) != nil {
			return false
		}
		return payload["type"] == "verify" && VerifyTurnstileToken(ctx, payload["token"])
	}()
	if !verified {
		sendJSON(ctx, conn, Frame{"status": "verification_error", "error": "Verification failed. Please refresh verification and try again."})
		conn.Close(websocket.StatusPolicyViolation, "Verification required")
	}
	return verified
}

// ServeHTTP handles one WebSocket connection.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !originAllowed(r.Header.Get("Origin")) {
		http.Error(w, "Failed to open a WebSocket connection: invalid Origin header.", http.StatusForbidden)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true}) // origin checked above
	if err != nil {
		return
	}
	conn.SetReadLimit(1 << 20)
	defer conn.CloseNow()
	ctx := r.Context()
	if !authenticate(ctx, conn) {
		return
	}

	// Reject a new connection if one is already active
	s.mu.Lock()
	if s.active != nil && s.active != conn {
		s.mu.Unlock()
		sendJSON(ctx, conn, Frame{"error": "A connection is already active. Only one connection permitted at a time."})
		conn.Close(websocket.StatusNormalClosure, "")
		return
	}
	s.active = conn
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.active == conn {
			s.active = nil
		}
		s.mu.Unlock()
	}()

	send := func(frame Frame) error { return sendJSON(ctx, conn, frame) }
	sessionStart := time.Now()
	sessionLimit := time.Duration(s.SessionMinutes) * time.Minute
	var pending []map[string]any

	// Each connection is its own conversation
	if err := ResetConversation(); err != nil {
		send(Frame{"error": err.Error()})
		conn.Close(websocket.StatusInternalError, "agent unavailable")
		return
	}
	if send(Frame{"status": "verified"}) != nil {
		return
	}
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return
		}
		// Check if the session duration has been exceeded
		if time.Since(sessionStart) > sessionLimit {
			send(Frame{"error": fmt.Sprintf("Session duration limit (%d minutes) exceeded. Connection will be closed.", s.SessionMinutes)})
			conn.Close(websocket.StatusNormalClosure, "Session duration limit exceeded")
			return
		}
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			if send(Frame{"error": "Invalid JSON payload."}) != nil {
				return
			}
			continue
		}
		response, courses, err := HandleMessage(ctx, payload, pending, send)
		if err != nil {
			courses = nil
			response = Frame{"error": err.Error()}
		}
		pending = courses
		if send(response) != nil {
			return
		}
	}
}

// Run starts the WebSocket server (TLS when WSS_ENABLED).
func Run(ctx context.Context, host string, port int) error {
	wss := env.True(env.Get("WSS_ENABLED", "false"))
	certFile, keyFile := os.Getenv("WSS_CERTFILE"), os.Getenv("WSS_KEYFILE")
	if wss && (certFile == "" || keyFile == "") {
		return errors.New("WSS_ENABLED requires WSS_CERTFILE and WSS_KEYFILE to be set.")
	}
	// CANVAS_SANDBOX=yes: capture Canvas first if needed, then answer every Canvas call locally
	if _, err := sandbox.Start(ctx, nil); err != nil {
		return err
	}
	server := &http.Server{Addr: fmt.Sprintf("%s:%d", host, port), Handler: NewServer()}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}()
	var err error
	if wss {
		err = server.ListenAndServeTLS(certFile, keyFile)
	} else {
		err = server.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
