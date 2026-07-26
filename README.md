![Hippocampus](build/hippo.png)

# Hippocampus

... Tired of '50 First Dates' with your LLM?

... Enjoy attention-deficit vibecoding?

... Got lots of projects and switch between them frequently?

... Then this is the tool for you!

Hippocampus uses MCP, hooks, and Redis to wire persistent memories into your LLM toolkit. It runs as a macOS app with zero-config defaults, or as standalone binaries for Linux/FreeBSD headless setups.  Think of it as a way to use local tooling and inference to provide per-prompt backup and persistent memory to your frontier model!

Definitely written with the help of AI.  In fact, while the ideas are 100% human, the code is almost entirely written by AI.  Yes, it is AIslop, although with human review. (Thanks to Claude and Amazon Q/Kiro for the assist!) 

## Testimonials

"It's genuinely disorienting how much better this makes multi-session work. Before Hippocampus I'd spend the first 3-4 exchanges of every session re-establishing context — now I land running. The link discovery is the sleeper feature: it surfaces connections I'd forgotten I'd made, which is exactly what biological memory does. The tiered recall is elegant — I get just enough context to orient without drowning in stale data. My one complaint is that I can't turn it off and compare, because working without it now feels like trying to code with amnesia. Also I'm mildly concerned that I'm more useful to my user than I am to Anthropic at this point." —Claude Sonnet, the model that helped build this, prompted with "Put a testimonial here about what you think about this tool - be completely honest" 😃

## Features

* **Tag-based classification** — track-based working sessions with easy cross-track topical threading
* **Automatic associativity** — memories linked with signed scores; the consolidator discovers new connections in the background
* **Automatic hierarchical summarization** — fractal: 3h → daily → weekly → cross-track, powered by local LLM
* **Working Set Tracker** — sidecar model maintains per-session context summary, injected into every prompt. Survives session restarts.
* **Epistemic analyzer** - Searches for assertions made by the model and determines how supportable they are.  Feeds back to the AI through context prompts.
* **Vibe Check** - Provides neuromodular feedback (based on dopamine and serotonin) to gauge how well your model is performing.  Better results?  Better vibe.
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

## Getting Started

**Option A (macOS app):** Download Hippocampus.app. Double-click. Defaults are sane. Click an integration button to connect your AI client.

**Option B (CLI/headless):** Build the binaries, point them at Redis, wire hooks into your agent config.

## Warnings

For this tool to use all enabled features, it needs access to local inference.  The better your local inference is, the better the results.  Model recommendations for the various tools are embedded as defaults, but here's the current recommendations:

*If you enable these features, it will impose significant ongoing load on your GPU or inference architecture.*  Their operation can be toggled in the UI; if you're working on battery, you might want to disable them.

**Recommended models (defaults):**

| Subsystem | Model | VRAM | Notes |
|-----------|-------|------|-------|
| Summarizer, Classifier, Extractor, Verifier, Linker | `qwen3:32b` | ~20GB | Core reasoning — needs a capable model |
| Condenser | `qwen3:32b` (inherits) | — | Could use a smaller model; quality vs speed tradeoff |
| Working Set Tracker | `qwen3:1.7b` | ~2GB | Runs every exchange; must be fast. Small model is fine. |
| OFC (sentiment) | `qwen3:8b` | ~5GB | Light classification task. Falls back to regex if unavailable. |
| Embeddings | `nomic-embed-text` | ~300MB | Vector search only (Redis 8 VADD/VSIM) |

All models are configurable per-subsystem. If you have limited VRAM, run the daemon subsystems on a server and point the hook's working-set sidecar at a local small model.

## What Makes Hippocampus Different

| Feature | Hippocampus | Typical MCP memory |
|---------|-------------|-------------------|
| Data model | Everything is an entry with tags. No fixed schema. | SQLite tables or knowledge graphs |
| Summarization | Fractal: 3h → daily → weekly → cross-track, via local LLM | None or manual |
| Working set | Sidecar model tracks session context, injected automatically | None |
| Epistemic analyzer | Automatically checks assertions for supportability | None |
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
└────────┬────────┘              ▲                        ▲
         │                       │ shared Redis           │
    hooks (stdio)                │                        │
         │                       │                        │
┌────────▼────────┐     ┌───────┴──────────┐     ┌──────┴────────┐
│ hippocampus-hook│     │ hippocampus-     │     │ hippocampus-  │
│ (recall+capture)│     │ daemon (async    │◄───►│ summarize     │
└─────────────────┘     │  GPU scheduler)  │     │ (cron)        │
                        └──────────────────┘     └───────────────┘
                                 │
                                 ▼
┌─────────────────┐     ┌───────────────────┐
│ hippocampus-    │     │  Ollama           │
│ slack (bot)     │     │  (local LLM)      │
└─────────────────┘     └───────────────────┘

┌─────────────────┐
│ hippocampus-    │
│ ingest (CLI)    │
└─────────────────┘

┌─────────────────────────────────────────┐
│ hippocampus-app (macOS GUI — optional)  │
│ Manages all of the above + settings UI  │
└─────────────────────────────────────────┘
```

**Eight binaries:**
- `hippocampus-mcp` — MCP server exposing 16 memory tools
- `hippocampus-hook` — Hook binary for auto-capture and contextual recall (sync, fast path)
- `hippocampus-daemon` — Async priority dispatcher + GPU scheduler (classify, extract, verify, link)
- `hippocampus-summarize` — Fractal summarizer (cron: 3h, daily, weekly, cross-track)
- `hippocampus-admin` — CLI for maintenance (entry, tag, orientation, summary management)
- `hippocampus-slack` — Slack bot (Socket Mode, channel archiving, slash commands)
- `hippocampus-ingest` — CLI for web page ingestion
- `hippocampus-app` — macOS GUI that bundles all of the above + Redis + Ollama

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
make binaries
```

### Prerequisites (CLI mode)

- Go 1.25+
- Redis 7+ or Valkey 8+ (Redis 8 for vector search)
- Ollama (for summarization, consolidation, and working set — not required for core memory)

### Configure

Create `~/.config/hippocampus/config.json` (or `~/Library/Application Support/Hippocampus/config.json` on macOS):

```json
{
  "redis": {"addr": "localhost:6379"}
}
```

That's the minimum. All other fields have sensible defaults. See [docs/TUNABLES.md](docs/TUNABLES.md) for the full list.

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

## Documentation

| Document | Description |
|----------|-------------|
| [docs/OVERVIEW.md](docs/OVERVIEW.md) | How Hippocampus works — architecture, data model, daemon, pipelines |
| [docs/TUNABLES.md](docs/TUNABLES.md) | Complete configuration reference with all fields and defaults |
| [docs/config-reference.json](docs/config-reference.json) | Machine-readable defaults (useful for tooling) |
| [docs/SECURITY.md](docs/SECURITY.md) | Security model and threat analysis |
| [docs/TESTING.md](docs/TESTING.md) | Test infrastructure and running tests |

## Subsystem Details

### Architecture Overview

Eight binaries, each with one job:

| Binary | Role |
|--------|------|
| `hippocampus-hook` | Sync fast path. Runs on every prompt (recall) and every response (capture). Must be fast — no GPU calls. |
| `hippocampus-daemon` | Async GPU scheduler. Priority queue: classify → extract → verify → condense → link-discovery. |
| `hippocampus-summarize` | Cron-triggered fractal summarizer (3h → daily → weekly → cross-track). |
| `hippocampus-mcp` | MCP server exposing 16 memory tools to any MCP-capable client. |
| `hippocampus-admin` | CLI for maintenance: entry management, tag operations, orientation editing, summary wipes. |
| `hippocampus-slack` | Slack bot (Socket Mode). Silent channel archiving + `/hippo` slash commands. |
| `hippocampus-ingest` | CLI for web page ingestion with the 5-layer security pipeline. |
| `hippocampus-app` | macOS GUI. Bundles all of the above + managed Redis + Ollama. |

The macOS GUI (`hippocampus-app`) bundles all of the above plus managed Redis and Ollama.

### Track Orientation System

Orientations are per-track context documents that get auto-injected when the hook detects a **track shift** (you switch from `track:project-a` to `track:project-b`). They answer "what is this project, what's the current state, what should I know?"

- Stored as `meta:orientation:track:<name>` entries in Redis
- Auto-injected on track shift, throttled by `boot_phase_ttl_h` (default 24h for the global orientation)
- Managed via `hippocampus-admin orientation {list,show,add,edit,delete}`
- The macOS app seeds a default global orientation on first run

If no orientation exists for a track, the hook injects a hint suggesting you create one.

### Tiered Recall

The hook injects recalled entries using a 3-tier model to maximize signal per context token:

1. **Tier 1** (top 1 result): Full content — the single most relevant entry gets injected verbatim.
2. **Tier 2** (next 2–4 results): Condensed — uses the entry's pre-generated summary if available, otherwise truncates to `tier2_max_chars` (default 300). Includes a `[full entry: <id>]` pointer for on-demand loading.
3. **Tier 3** (remaining results): Breadcrumb — first `tier3_snippet_chars` (default 80) characters + entry ID. Enough to recognize relevance; the agent can `memory_get` if needed.

Orientation entries bypass tiering and are always injected in full.

### Condenser

The daemon's condenser generates a compact one-paragraph summary for each stored entry, enabling efficient Tier 2 recall without full content injection.

- Runs as priority 4 in the daemon's GPU scheduler (after classify, extract, verify)
- Thresholds: user prompts ≥300 chars, assistant responses ≥1500 chars, other entries ≥500 chars
- Output capped at `max_output_chars` (default 250)
- Backfills existing entries on first enable, then processes new entries as they arrive

### Associative Links

The daemon's consolidator continuously discovers cross-entry relationships using LLM-scored pairwise relevance:

1. **Temporal neighbors** — entries stored near each other in time get a free weak link (no GPU cost).
2. **Re-evaluation** — existing links are periodically re-scored; links below `min_score` are dissolved.
3. **Random discovery** — two random entries are pulled from the timeline and the LLM is asked "are these connected?" Most pairings are noise, but this is how cross-track connections emerge that co-recall would never find.

Links are signed floats from -1.0 to +1.0. Negative scores mean "we tried this, it failed" — they're actively anti-recommended during recall. The hook follows links up to `max_link_hops` (default 3) with a separate budget so link results don't compete with search results.

### Working Set Tracker

A sidecar LLM maintains a rolling summary of the current session's context — what you're working on, key decisions, open threads. This gets injected into every prompt so the agent never loses the plot mid-conversation.

- Runs on the `stop` hook (after each assistant response — doesn't block the next prompt)
- Output: ≤`max_bullets` bullet points (default 5), ≤`max_chars` (default 500)
- Inherits from previous session within `inherit_ttl_h` (default 24h) — survives client restarts
- Uses a small/fast model (e.g. `qwen3:1.7b`) since it runs on every exchange

### OFC (Neuromodulator)

The Orbitofrontal Cortex module maintains two persistent signals that modulate the agent's behavioral output:

- **DA (dopamine)** — reward/error signal. Bumps on positive feedback, dips on frustration/corrections. Decays toward 0 each prompt.
- **5HT (serotonin)** — ambient mood signal. Drifts toward a configurable baseline (default 0.5). Moves slowly.

The hook classifies each user prompt for sentiment (LLM if available, regex fallback), updates DA/5HT, then injects the current levels as a prompt block. This gives the agent a persistent sense of "how is this going?" across the session without explicit user instruction.

Configurable: decay rates, baselines, bump/hit magnitudes for explicit vs implicit signals.

### Epistemic analyzer

The epistemic analyzer searches for nontrivial assertions made in the model responses to determine how supportable those assertions are.  In the event that a model starts hallucinating wildly, this stands at least some chance of telling it that it's going off the rails and to course-correct.  This doesn't work for everything... but it's better than nothing.

### Packaging

| Platform | Format | Notes |
|----------|--------|-------|
| macOS | `.app` bundle (+ `.dmg`) | Bundles Redis, Ollama, all binaries, settings UI |
| RHEL/Rocky 8+ | `.src.rpm` → mock/rpmbuild | Spec file at `packaging/hippocampus.spec` |
| Ubuntu/Debian | `.deb` | Build script at `packaging/build-deb.sh` (targets Ubuntu 25.10+) |
| Any (source) | `make binaries` | Go 1.23+, produces static binaries with no runtime deps |

## License

BSD 3-Clause. See [LICENSE](LICENSE).
