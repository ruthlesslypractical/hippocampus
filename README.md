# Hippocampus

...Tired of '50 First Dates' with your LLM?

...Enjoy attention-deficit vibecoding?

...Got lots of projects and switch between them frequently?

Then this is the tool for you!

Hippocampus uses MCP, hooks, and Redis to wire persistent memories into your LLM toolkit. It runs as a macOS app with zero-config defaults, or as standalone binaries for Linux/FreeBSD headless setups.

Definitely written with the help of AI.  In fact, while the ideas are 100% human, the code is almost entirely written by AI.  Yes, it is AIslop, although with human review. (Thanks to Claude and Amazon Q/Kiro for the assist!)

## Features

* **Tag-based classification** — track-based working sessions with easy cross-track topical threading
* **Automatic associativity** — memories linked with signed scores; the consolidator discovers new connections in the background
* **Automatic hierarchical summarization** — fractal: 3h → daily → weekly → cross-track, powered by local LLM
* **Working Set Tracker** — sidecar model maintains per-session context summary, injected into every prompt. Survives session restarts.
* **Lean kernel context management** — page-fault model keeps your context window free for work, not orientation
* **One-click integration** with Claude Desktop, Claude Code, Cursor, Kiro CLI, Windsurf, and Gemini CLI
* **Network sharing** — share memory between machines on your LAN with TLS + 6-word passphrase auth
* **Ollama model management** — pull, delete, and assign models to roles from the Settings UI
* **Slack integration** — silent channel archiving + `/hippo` slash commands (search, store, ingest, forget, link, tags)
* **Web page ingestion** with 5-layer security model (extraction → injection scanning → tagging → untrusted framing → pointer/stub model)
* **Continuous pairwise relevance consolidator** — background process discovers associative links between entries
* **Full-text search** (RediSearch) and **vector similarity search** (Redis 8 VADD/VSIM)
* **macOS GUI app** — bundled Redis and Ollama, settings UI, data browser, tag editor
* **Confidence decay** — unreinforced memories fade over configurable half-life

## How It Works

**Option A (macOS app):** Download Hippocampus.app. Double-click. Defaults are sane. Click an integration button to connect your AI client.

**Option B (CLI/headless):** Build the binaries, point them at Redis, wire hooks into your agent config.

## What Makes Hippocampus Different

| Feature | Hippocampus | Typical MCP memory |
|---------|-------------|-------------------|
| Data model | Everything is an entry with tags. No fixed schema. | SQLite tables or knowledge graphs |
| Summarization | Fractal: 3h → daily → weekly → cross-track, via local LLM | None or manual |
| Working set | Sidecar model tracks session context, injected automatically | None |
| Associative links | Signed scores (-1.0 to +1.0). Negative = "we tried this, it failed" | Unweighted or none |
| Context management | Lean kernel / demand paging (page-fault model) | Dump everything or naive RAG |
| Link discovery | Continuous background consolidator (LLM-scored pairwise relevance) | Manual only |
| Network sharing | TLS + 6-word passphrase (72-bit entropy), zero config | None |
| Web ingestion | 5-layer security: sanitize → scan → tag → frame → pointer model | None or naive |
| Slack | Silent archive + slash commands (search, store, ingest, forget) | None |
| Backend | Redis/Valkey (sets, sorted sets, FT.SEARCH, vector sets) | SQLite |
| Language | Go (single static binary, no runtime deps) | Python/Node |

## Architecture

```
┌─────────────────┐     ┌──────────────────┐     ┌───────────────┐
│  AI Client      │     │  hippocampus-mcp │     │  Redis/Valkey │
│  (Claude, Kiro, │◄───►│  (MCP server)    │◄───►│               │
│   Gemini, etc.) │     └──────────────────┘     └───────────────┘
└────────┬────────┘              ▲
         │                       │
    hooks (stdio)                │ shared Redis
         │                       │
┌────────▼────────┐     ┌───────┴──────────┐     ┌───────────────┐
│ hippocampus-hook│     │ hippocampus-     │     │  Ollama       │
│ (recall+capture)│     │ summarize        │◄───►│  (local LLM)  │
└─────────────────┘     │  (--consolidate) │     └───────────────┘
                        └──────────────────┘
┌─────────────────┐
│ hippocampus-    │
│ slack (bot)     │
└─────────────────┘

┌─────────────────────────────────────────┐
│ hippocampus-app (macOS GUI — optional)  │
│ Manages all of the above + settings UI  │
└─────────────────────────────────────────┘
```

**Five binaries:**
- `hippocampus-mcp` — MCP server exposing 16 memory tools
- `hippocampus-hook` — Hook binary for auto-capture and contextual recall
- `hippocampus-summarize` — Fractal summarizer + consolidator (`--consolidate` flag)
- `hippocampus-slack` — Slack bot (Socket Mode, channel archiving, slash commands)
- `hippocampus-ingest` — CLI for web page ingestion

Plus `hippocampus-app` — macOS GUI that bundles all of the above + Redis + Ollama.

## Quickstart

### Option A: macOS App (zero-config)

1. Download `Hippocampus.app` from Releases
2. Open it — Redis starts automatically (local mode)
3. Click an integration button (Claude, Kiro, Cursor, Gemini, Windsurf)
4. Start a conversation — your AI now has persistent memory

### Option B: Build from Source

```bash
git clone https://github.com/ruthlesslypractical/hippocampus
cd hippocampus
./build.sh
```

Or build individual binaries:

```bash
go build -o bin/hippocampus-mcp ./cmd/mcp-server/
go build -o bin/hippocampus-hook ./cmd/hook/
go build -o bin/hippocampus-summarize ./cmd/summarize/
go build -o bin/hippocampus-slack ./cmd/slack/
go build -o bin/hippocampus-ingest ./cmd/ingest/
```

### Prerequisites (CLI mode)

- Go 1.23+
- Redis 7+ or Valkey 8+ (Redis 8 for vector search)
- Ollama (for summarization, consolidation, and working set — not required for core memory)

### Configure

Create `~/.config/hippocampus/config.json` (or `~/Library/Application Support/Hippocampus/config.json` on macOS):

```json
{
  "redis": {"addr": "localhost:6379"}
}
```

That's the minimum. All other fields have sensible defaults. See [Configuration Reference](#configuration-reference) for the full list.

### Wire into Your Agent

Example agent config (Kiro CLI):

```json
{
  "name": "my-agent",
  "hooks": {
    "userPromptSubmit": [{"command": "/path/to/hippocampus-hook", "timeout_ms": 3000}],
    "stop": [{"command": "/path/to/hippocampus-hook", "timeout_ms": 3000}]
  },
  "mcpServers": {
    "hippocampus": {
      "command": "/path/to/hippocampus-mcp",
      "args": ["--config", "~/.config/hippocampus/config.json"]
    }
  }
}
```

### Schedule Summarization

The macOS app handles this automatically. For CLI setups:

```bash
# Every 3 hours
hippocampus-summarize --3h

# Daily rollup
hippocampus-summarize --daily

# Weekly rollup
hippocampus-summarize --weekly

# Cross-track theme detection
hippocampus-summarize --cross-track

# Run consolidator (discovers associative links between entries)
hippocampus-summarize --consolidate
```

## Working Set Tracker

A lightweight sidecar model maintains a running summary of what you're working on in the current session. This summary is injected into every prompt — so the LLM always knows context without burning tokens on full recall.

Key behaviors:
- Runs a small local model (default: `qwen3:1.7b`) alongside your main LLM
- Produces a concise bullet-point summary of session context
- Survives session restarts: if a previous session ended less than 24h ago, the new session inherits its working set
- Completely optional — disabled by default

Enable it:

```json
{
  "working_set": {
    "enabled": true
  }
}
```

## Network Sharing

In local Redis mode, check **"Share on local network"** in settings to let other machines connect to your memory store.

What happens:
1. Self-signed TLS certificate is generated automatically
2. Redis binds to `0.0.0.0` instead of localhost
3. A 6-word passphrase is displayed (72 bits of entropy from a 4096-word EFF-derived list) along with a verify phrase and your local IPs
4. Other machines connect using the passphrase for Redis AUTH over TLS

No port forwarding, no manual cert management, no accounts. Just a passphrase.

## Ollama Model Management

The Settings UI provides model management:
- View all installed models
- Pull new models (with progress bar, supports concurrent downloads)
- Delete models you no longer need
- Assign models to roles: **summarizer**, **embedding**, and **working set tracker**

## Slack Integration

The Slack bot silently archives channel conversations and provides `/hippo` slash commands:

| Command | Description |
|---------|-------------|
| `/hippo search <query>` | Search memory |
| `/hippo recent [N]` | Last N entries (newest first) |
| `/hippo tags [filter]` | List tags with counts |
| `/hippo store <text>` | Store a memory (trusted) |
| `/hippo forget <#>` | Delete entry (with confirmation) |
| `/hippo link <#a> <#b> <score>` | Link two entries |
| `/hippo ingest <url>` | Ingest a web page |
| `/hippo status` | Bot status |

Setup: Create a Slack app with Socket Mode, add to channels, configure tokens in Settings.

## Data Model

Everything is an **entry** with **tags**:

```
Entry {
  id:        string
  timestamp: time
  content:   string
  tags:      []string
}
```

Tags do all the organizational work:
- `track:ProjectName` — major topics/projects
- `summary:track:Name` — distilled track summaries
- `summary:3h`, `summary:daily`, `summary:weekly` — temporal summaries
- `kind:user_prompt`, `kind:assistant_response` — conversation history
- `auto:captured` — hook-captured entries
- `date:2026-07-17` — temporal scoping
- `session:<uuid>` — session grouping
- `source:slack`, `source:web` — provenance
- `content:slack`, `content:full` — untrusted content (excluded from auto-recall)
- Any arbitrary tag you want

### Associative Links

Entries can be linked with signed scores:
- `+0.5 to +1.0` — relevant/supporting ("this extends that")
- `-0.5 to -1.0` — anti-relevant ("we tried this and it FAILED")

The recall hook follows links during context retrieval. Negative links surface prior failures to prevent repeating mistakes. The consolidator discovers new links in the background.

## MCP Tools (16)

| Tool | Description |
|------|-------------|
| `memory_store` | Store an entry with tags |
| `memory_search` | Full-text search (FT.SEARCH or fallback) |
| `memory_by_tags` | Retrieve by tag intersection/union |
| `memory_get` | Get a specific entry by ID |
| `memory_delete` | Remove an entry |
| `memory_add_tags` | Add tags to an existing entry |
| `memory_remove_tags` | Remove tags from an entry |
| `memory_list_tags` | List all tags with counts |
| `memory_rename_tag` | Rename a tag across all entries |
| `memory_time_range` | Query entries by time window |
| `memory_link` | Create associative link (scored, optionally typed) |
| `memory_unlink` | Remove a link |
| `memory_links` | Get all links for an entry |
| `memory_ingest_url` | Ingest a web page (5-layer security) |
| `memory_store_chunked` | Store large content as auto-split chunks |
| `memory_get_section` | Retrieve specific chunk by index |

## Context Management: The Lean Kernel

Hippocampus uses a two-phase orientation strategy:

1. **First prompt** in a session: inject full orientation (who you are, how memory works, available tracks)
2. **Subsequent prompts**: inject only the lean kernel (~200 tokens) — just enough to remind the model it has memory tools available

This keeps 95%+ of context free for actual work. The model "page-faults" into memory on demand using the MCP tools.

See `prompts/` for template orientation entries to customize.

## Web Ingestion Security Model

When a web page is ingested (via MCP tool or `/hippo ingest`):

| Layer | Protection |
|-------|-----------|
| L1 | Extraction sanitization (strips scripts, ads, invisible elements) |
| L2 | Prompt injection scanning (regex + density heuristic, risk score 0–1.0) |
| L3 | Source tagging (`source:web`, `url:*`, `domain:*`) |
| L4 | Untrusted framing (⚠️ warning when content is loaded) |
| L5 | Pointer/stub model (full content never auto-injected into context) |

Content is stored but **never automatically surfaced** — the model must explicitly request it via `memory_get`.

## Configuration Reference

### Redis

| Key | Default | Description |
|-----|---------|-------------|
| `redis.addr` | `localhost:6379` | Redis/Valkey host:port |
| `redis.password` | `""` | AUTH password |
| `redis.db` | `0` | Database number |
| `redis.tls` | `false` | Enable TLS |
| `redis.pool_size` | `10` | Connection pool size |

### Memory

| Key | Default | Description |
|-----|---------|-------------|
| `memory.recall_max_chars` | `8000` | Max chars injected per prompt |
| `memory.recall_max_entries` | `10` | Max entries injected per prompt |
| `memory.recall_scan_depth` | `100` | Scan depth for naive fallback |
| `memory.store_max_chars` | `0` | Max chars per entry (0 = unlimited) |
| `memory.store_min_prompt_len` | `20` | Min prompt length to auto-store |
| `memory.store_min_response_len` | `100` | Min response length to auto-store |
| `memory.decay_half_life_days` | `30` | Confidence half-life (0 = no decay) |

### Ollama

| Key | Default | Description |
|-----|---------|-------------|
| `ollama.base_url` | `http://localhost:11434` | Ollama API endpoint |
| `ollama.model` | `qwen3:32b` | Model for summarization |
| `ollama.embedding_model` | `nomic-embed-text` | Model for vector embeddings |
| `ollama.embedding_dimensions` | `768` | Vector dimensions |
| `ollama.timeout_minutes` | `10` | HTTP timeout |

### Working Set

| Key | Default | Description |
|-----|---------|-------------|
| `working_set.enabled` | `false` | Enable working set tracker |
| `working_set.model` | `qwen3:1.7b` | Sidecar model for context tracking |
| `working_set.max_bullets` | `5` | Max bullet points in working set |
| `working_set.max_chars` | `500` | Max working set size in chars |
| `working_set.inherit_ttl_h` | `24` | Inherit from previous session within this window |

### Ingest

| Key | Default | Description |
|-----|---------|-------------|
| `ingest.reject_threshold` | `0.8` | Risk score to reject page |
| `ingest.sanitize_threshold` | `0.5` | Risk score to sanitize |
| `ingest.max_chunk_size` | `3000` | Max chars per chunk |
| `ingest.fetch_timeout_s` | `30` | HTTP fetch timeout |

### Consolidation

| Key | Default | Description |
|-----|---------|-------------|
| `consolidation.pairs_per_run` | `10` | Pairs evaluated per cycle |
| `consolidation.min_score` | `0.4` | Min score to create link |
| `consolidation.cycle_pause_s` | `600` | Seconds between cycles |
| `consolidation.temperature` | `0.1` | LLM temperature for scoring |

### Hook

| Key | Default | Description |
|-----|---------|-------------|
| `hook.timeout_s` | `5` | Redis operation timeout |
| `hook.boot_phase_ttl_h` | `24` | Hours before re-injecting full orientation |
| `hook.max_link_hops` | `3` | Associative link traversal depth |

### MCP

| Key | Default | Description |
|-----|---------|-------------|
| `mcp.default_search_limit` | `10` | Default search results |
| `mcp.default_tag_limit` | `20` | Default tag query results |
| `mcp.default_time_range_limit` | `20` | Default time range results |

### Slack

| Key | Default | Description |
|-----|---------|-------------|
| `slack.bot_token` | `""` | Slack bot token (xoxb-...) |
| `slack.app_token` | `""` | Slack app token (xapp-...) |
| `slack.channels` | `[]` | Channels to monitor (`[{"id":"...","name":"...","mode":"archive"}]`) |

## Environment Variables

- `HIPPOCAMPUS_CONFIG` — path to config file (overrides default search)
- `KIRO_SESSION_ID` — session identifier (set by the calling client)

## License

BSD 3-Clause. See [LICENSE](LICENSE).
