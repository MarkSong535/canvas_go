# Repository Layout

```
canvas_go/
├── cmd/
│   ├── canvas_chat/            # Interactive console
│   ├── ws_server/              # Authenticated WebSocket bridge
│   ├── file_index_downloader/  # Bulk download + optional vector store upload
│   ├── canvas_sandbox/         # Sandbox sync / status
│   ├── vector_store_search/    # Interactive vector store search
│   └── ws_smoke/               # Smoke-test client for a running ws_server
├── internal/
│   ├── agent/                  # ReAct agent loop, prompts, model routing, UI event stream
│   ├── canvastools/            # Canvas, vector store and LaTeX-check tools
│   ├── config/                 # Agent configuration (model, routing tiers, tool list)
│   ├── models/                 # Chat messages, OpenAI model, model manager (price cap)
│   ├── openai/                 # OpenAI-compatible REST client (chat, files, vector stores)
│   ├── memory/                 # Agent memory steps and how they replay as messages
│   ├── tools/                  # Tool interface, argument binding, final_answer tool
│   ├── sandbox/                # Canvas sandbox: capture, store, local Canvas look-alike
│   ├── filetext/               # Text from PDF, Word, PowerPoint, Excel, notebooks, HTML
│   ├── latexsvg/               # Server-side LaTeX/chemfig -> SVG rendering
│   ├── wsserver/               # WebSocket handlers
│   ├── chat/                   # Terminal chat UI
│   ├── downloader/             # File index downloader
│   └── ...                     # logger, env (.env loading), jsonval, textutil, json5, agenterr
├── examples/                   # Guided demos (go run ./examples/<name>)
├── tests/                      # End-to-end tests (simulated tests)
├── web/                        # React client (Vite + TypeScript)
├── Dockerfile
└── env_example.txt
```