// Command ws_server is the authenticated WebSocket bridge between the web client and the Canvas agent.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"canvas_go/internal/env"
	"canvas_go/internal/wsserver"
)

func main() {
	env.Load()
	host := env.Get("CANVAS_WS_HOST", "0.0.0.0")
	port, err := strconv.Atoi(env.Get("CANVAS_WS_PORT", "8765"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "CANVAS_WS_PORT must be a number")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := wsserver.Run(ctx, host, port); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
