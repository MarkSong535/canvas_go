// Command ws_smoke smoke-tests a running ws_server: it connects with the page's Origin, verifies, asks one or
// more questions and prints the streamed frames and the final answers.
//
// Turnstile tokens come from the browser widget, so a script can't get a real one. For a staging deployment,
// start ws_smoke with -simulated-turnstile 127.0.0.1:9999 and run the server with
// TURNSTILE_VERIFY_URL=http://127.0.0.1:9999: the stand-in verifier accepts only the -token given here.
//
//	go run ./cmd/ws_smoke -url ws://127.0.0.1:8765/ -simulated-turnstile 127.0.0.1:9999 -query "What courses am I in?"
//
// With -serve-only it only runs the stand-in verifier (until stopped), e.g. for manual testing from the web
// client's dev build, whose VITE_SKIP_TURNSTILE=1 mode sends the token "dev-skip".
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

type queries []string

func (q *queries) String() string     { return strings.Join(*q, "; ") }
func (q *queries) Set(v string) error { *q = append(*q, v); return nil }

func main() {
	url := flag.String("url", "ws://127.0.0.1:8765/", "WebSocket URL of the server")
	origin := flag.String("origin", "https://markso.ng", "Origin header to send")
	token := flag.String("token", "smoke-token", "Turnstile token to verify with")
	simulated := flag.String("simulated-turnstile", "", "serve a stand-in Turnstile verifier on this address (host:port)")
	hostname := flag.String("hostname", "markso.ng", "hostname the stand-in verifier reports")
	action := flag.String("action", "canvas_ws", "action the stand-in verifier reports")
	timeout := flag.Duration("timeout", 3*time.Minute, "overall timeout")
	serveOnly := flag.Bool("serve-only", false, "only run the stand-in verifier (needs -simulated-turnstile)")
	escalate := flag.Bool("escalate", false, "after the questions, ask for a retry one tier up")
	var asks queries
	flag.Var(&asks, "query", "question to ask (repeatable; default: What courses am I in?)")
	flag.Parse()
	if len(asks) == 0 {
		asks = queries{"What courses am I in?"}
	}

	if *simulated != "" {
		listener, err := net.Listen("tcp", *simulated)
		if err != nil {
			fail("simulated turnstile: %v", err)
		}
		go http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.ParseForm()
			ok := r.Form.Get("response") == *token
			fmt.Printf("[simulated turnstile] token %q -> success=%v\n", r.Form.Get("response"), ok)
			json.NewEncoder(w).Encode(map[string]any{"success": ok, "hostname": *hostname, "action": *action})
		}))
	}

	if *serveOnly {
		if *simulated == "" {
			fail("-serve-only needs -simulated-turnstile")
		}
		fmt.Printf("stand-in Turnstile verifier on %s accepting token %q\n", *simulated, *token)
		select {}
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, *url, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {*origin}}})
	if err != nil {
		fail("connect: %v", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(16 << 20)
	send(ctx, conn, map[string]any{"type": "verify", "token": *token})
	if frame := read(ctx, conn); frame["status"] != "verified" {
		fail("verification failed: %v", frame)
	}
	fmt.Println("✓ verified")

	failed := false
	var requests []map[string]any
	for _, question := range asks {
		requests = append(requests, map[string]any{"type": "chat", "query": question})
	}
	if *escalate {
		requests = append(requests, map[string]any{"type": "escalate"})
	}
	for _, request := range requests {
		fmt.Printf("\n> %v\n", request)
		start := time.Now()
		send(ctx, conn, request)
		for {
			frame := read(ctx, conn)
			switch frame["type"] {
			case "route":
				fmt.Printf("[route] %v\n", frame["tier"])
			case "tool":
				fmt.Printf("[tool] %v\n", frame["name"])
			case "delta":
				fmt.Print(frame["text"])
			case "retract":
				fmt.Println("\n[retract]")
			default:
				if errText, ok := frame["error"]; ok {
					fmt.Printf("\n✗ error: %v\n", errText)
					failed = true
				} else {
					renders, _ := frame["renders"].(map[string]any)
					fmt.Printf("\n\n[answer] tier=%v can_escalate=%v renders=%d (%.1fs)\n%v\n",
						frame["tier"], frame["can_escalate"], len(renders), time.Since(start).Seconds(), frame["answer"])
				}
				goto next
			}
		}
	next:
	}
	conn.Close(websocket.StatusNormalClosure, "")
	if failed {
		os.Exit(1)
	}
}

func send(ctx context.Context, conn *websocket.Conn, frame map[string]any) {
	data, _ := json.Marshal(frame)
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		fail("send: %v", err)
	}
}

func read(ctx context.Context, conn *websocket.Conn) map[string]any {
	_, data, err := conn.Read(ctx)
	if err != nil {
		fail("read: %v", err)
	}
	var frame map[string]any
	if err := json.Unmarshal(data, &frame); err != nil {
		fail("bad frame %q: %v", data, err)
	}
	return frame
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "✗ "+format+"\n", args...)
	os.Exit(1)
}
