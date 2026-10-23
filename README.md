# Canvas Student Agent (Go)

An OpenAI-powered ReAct agent that works as a Canvas LMS companion.

[Code Structure](Structure.md)

## Prerequisites

- Go 1.26+
- A Canvas LMS account with student-level API access
- An OpenAI API key (defaults to `gpt-6-luna`; see [Models](#models))
- Optional: `latex` (amsmath, mhchem, chemfig) and `dvisvgm` for server-side math rendering, and `catdoc` for old `.doc/.ppt/.xls` files (all installed in the Docker image)

## Running

```bash
go run ./cmd/canvas_chat                       # terminal chat
go run ./cmd/ws_server                         # WebSocket server (CANVAS_WS_HOST / CANVAS_WS_PORT, default 0.0.0.0:8765)
go run ./cmd/file_index_downloader             # download everything + optional vector upload
go run ./cmd/file_index_downloader --upload-only
go run ./cmd/canvas_sandbox sync               # capture Canvas into the sandbox
go run ./cmd/canvas_sandbox status
```

Or build all binaries into `bin/`:

```bash
go build -o bin/ ./cmd/...
```

The CLI commands are `help`, `new`, `deeper`, `status`, `examples`, `clear` and `exit`. Answers stream into the terminal as they are generated.

### Docker

```bash
docker build -t canvas-go .
docker run -d --name canvas-go -p 127.0.0.1:8765:8765 --env-file .env -v "$PWD/workdir:/app/workdir" canvas-go
```

## WebSocket Protocol

See `web/src/protocol.ts`. The page opens a WebSocket from `https://markso.ng`, verifies with `{ "type": "verify", "token": "<Turnstile token>" }` within 15 seconds, then sends `chat`, `escalate`, `reset` and `download` messages. Chat replies stream as `tool`, `route`, `delta` and `retract` frames, then one `answer` frame with server-rendered LaTeX in `renders`.

To test websocket deployment, run the server with `TURNSTILE_VERIFY_URL=http://127.0.0.1:9999` and:

```bash
go run ./cmd/ws_smoke -url ws://127.0.0.1:8765/ -simulated-turnstile 127.0.0.1:9999 -query "What courses am I in?"
```

---

## Canvas Sandbox Mode

Sandbox captures Canvas once into `CANVAS_SANDBOX_DIR` (default `workdir/canvas_sandbox`) and answer every Canvas call from a local simulated Canvas server, with writes recorded locally and file links rewritten to signed, expiring links. Everything lives in `CANVAS_SANDBOX_DIR`: `responses.json`, `files.json`, `manifest.json`, `files/`, `writes.jsonl` and `requests.jsonl`.

---

## Tests

```bash
go test ./...
go test -race ./...
```