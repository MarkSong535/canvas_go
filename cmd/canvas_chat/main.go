// Command canvas_chat is the interactive console: chat with the Canvas Student Agent in the terminal.
package main

import (
	"os"

	"canvas_go/internal/chat"
	"canvas_go/internal/env"
)

func main() {
	env.Load()
	os.Exit(chat.Main())
}
