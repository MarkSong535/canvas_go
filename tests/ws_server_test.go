package tests

// The WebSocket server over a real socket: origin check, Turnstile verification (against a simulated Cloudflare),
// streamed chat frames, and the one-connection limit.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/openai/openai-go/v3"

	"canvas_go/internal/latexsvg"
	"canvas_go/internal/wsserver"
)

func simulatedTurnstile(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		ok := r.Form.Get("secret") == "turnstile-secret" && r.Form.Get("response") == "good-token"
		json.NewEncoder(w).Encode(map[string]any{"success": ok, "hostname": "markso.ng", "action": "canvas_ws"})
	}))
}

func dial(t *testing.T, ctx context.Context, url, origin string) (*websocket.Conn, *http.Response, error) {
	header := http.Header{}
	if origin != "" {
		header.Set("Origin", origin)
	}
	return websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
}

func readFrame(t *testing.T, ctx context.Context, conn *websocket.Conn) map[string]any {
	t.Helper()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var frame map[string]any
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatal(err)
	}
	return frame
}

func writeFrame(t *testing.T, ctx context.Context, conn *websocket.Conn, frame map[string]any) {
	t.Helper()
	data, _ := json.Marshal(frame)
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatal(err)
	}
}

func TestWebSocketServerEndToEnd(t *testing.T) {
	turnstile := simulatedTurnstile(t)
	defer turnstile.Close()
	previous := wsserver.TurnstileVerifyURL
	wsserver.TurnstileVerifyURL = turnstile.URL
	defer func() { wsserver.TurnstileVerifyURL = previous }()
	t.Setenv("TURNSTILE_SECRET", "turnstile-secret")
	t.Setenv("TURNSTILE_HOSTNAMES", "markso.ng")
	t.Setenv("TURNSTILE_ACTION", "canvas_ws")

	a, _ := makeAgent([][]*openai.ChatCompletionChunk{
		toolTurn("w1"),
		{toolChunk(0, "w2", "final_answer_tool", `{"answer": "You have $x^2$ `), toolChunk(0, "", "", `courses."}`), usageChunk()},
	}, nil)
	wsserver.SetAgent(a)
	defer wsserver.SetAgent(nil)
	server := httptest.NewServer(wsserver.NewServer())
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Other origins are refused at the handshake
	if _, resp, err := dial(t, ctx, url, "https://evil.example"); err == nil || resp == nil || resp.StatusCode != 403 {
		t.Fatalf("a foreign origin must be refused: %v", err)
	}

	// A bad token is refused
	conn, _, err := dial(t, ctx, url, "https://markso.ng")
	if err != nil {
		t.Fatal(err)
	}
	writeFrame(t, ctx, conn, map[string]any{"type": "verify", "token": "bad-token"})
	if frame := readFrame(t, ctx, conn); frame["status"] != "verification_error" {
		t.Fatalf("frame: %v", frame)
	}
	conn.CloseNow()

	// A good token opens a session; chat streams frames, then the answer with server-rendered LaTeX
	conn, _, err = dial(t, ctx, url, "https://markso.ng")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	writeFrame(t, ctx, conn, map[string]any{"type": "verify", "token": "good-token"})
	if frame := readFrame(t, ctx, conn); frame["status"] != "verified" {
		t.Fatalf("frame: %v", frame)
	}

	// Only one connection at a time
	second, _, err := dial(t, ctx, url, "https://markso.ng")
	if err != nil {
		t.Fatal(err)
	}
	writeFrame(t, ctx, second, map[string]any{"type": "verify", "token": "good-token"})
	if frame := readFrame(t, ctx, second); !strings.Contains(frame["error"].(string), "already active") {
		t.Fatalf("second connection: %v", frame)
	}
	second.CloseNow()

	writeFrame(t, ctx, conn, map[string]any{"type": "chat", "query": "courses?"})
	var deltas strings.Builder
	var answer map[string]any
	sawTool := false
	for answer == nil {
		frame := readFrame(t, ctx, conn)
		switch frame["type"] {
		case "tool":
			sawTool = true
		case "delta":
			deltas.WriteString(frame["text"].(string))
		case "answer":
			answer = frame
		}
	}
	if !sawTool || deltas.String() != "You have $x^2$ courses." || answer["answer"] != "You have $x^2$ courses." {
		t.Fatalf("answer %v, deltas %q", answer, deltas.String())
	}
	renders, _ := answer["renders"].(map[string]any)
	if svg, ok := renders["I:x^2"].(string); hasLatex() && (!ok || !strings.HasPrefix(svg, "<svg")) {
		t.Fatalf("renders: %v", renders)
	}

	writeFrame(t, ctx, conn, map[string]any{"type": "nonsense"})
	if frame := readFrame(t, ctx, conn); frame["error"] != "Unsupported message type: nonsense" {
		t.Fatalf("frame: %v", frame)
	}
	writeFrame(t, ctx, conn, map[string]any{"type": "reset"})
	if frame := readFrame(t, ctx, conn); frame["status"] != "reset" {
		t.Fatalf("frame: %v", frame)
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	if frame := readFrame(t, ctx, conn); frame["error"] != "Invalid JSON payload." {
		t.Fatalf("frame: %v", frame)
	}
}

func hasLatex() bool { return latexsvg.Available() }

func TestAllowedOriginsSetting(t *testing.T) {
	server := httptest.NewServer(wsserver.NewServer())
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t.Setenv("WS_ALLOWED_ORIGINS", "http://localhost:5173, https://markso.ng")
	conn, _, err := dial(t, ctx, url, "http://localhost:5173")
	if err != nil {
		t.Fatalf("a listed origin is accepted: %v", err)
	}
	conn.CloseNow()
	if _, resp, err := dial(t, ctx, url, "https://evil.example"); err == nil || resp == nil || resp.StatusCode != 403 {
		t.Fatal("unlisted origins are still refused")
	}
}
