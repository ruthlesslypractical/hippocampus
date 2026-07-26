# How Hippocampus Works

A guide for the reasonably intelligent developer who knows Go, Redis, and LLMs but hasn't seen this codebase before.

## The Big Picture

Every AI chat tool has the same problem: the model forgets everything the moment the session ends. You explain your project structure, your preferences, your decisions — and next conversation it's back to square one. 50 First Dates, forever.

Hippocampus fixes this by wiring persistent memory into the LLM's tool loop. It captures conversations automatically, recalls relevant context on each prompt, and runs background processes to classify, link, and summarize entries over time.

**Why Redis?** Sets, sorted sets, FT.SEARCH, and now vector sets (VADD/VSIM in Redis 8). The data model maps cleanly: entries as hashes, tags as sets, timeline as a sorted set by timestamp, links as sorted sets by score. Sub-millisecond reads on the recall hot path. No ORM, no migrations.

**Why local LLM (Ollama)?** Summarization, classification, and epistemic extraction need an LLM but don't need GPT-4. A 32B quantized model on your GPU handles this fine, keeps data local, and costs nothing. Different subsystems can use different models (1.7B for working set, 32B for extraction).

## Binary Layout

Eight binaries, each with a clear job and lifecycle:

| Binary | Mode | When it runs | What it does |
|--------|------|-------------|--------------|
| `hippocampus-hook` | Sync | Every prompt + response | Fast-path recall and capture. Must return in <3s. |
| `hippocampus-daemon` | Async | Long-running service | Priority dispatcher: classification, extraction, verification, condensation, linking |
| `hippocampus-mcp` | Sync | Long-running (MCP server) | Exposes 16 memory tools to the AI client |
| `hippocampus-summarize` | Cron | Scheduled (3h/daily/weekly) | Fractal summarization only |
| `hippocampus-admin` | CLI | On demand | Entry/tag/orientation/summary management |
| `hippocampus-slack` | Async | Long-running (bot) | Channel archiving + slash commands |
| `hippocampus-ingest` | CLI | On demand | Web page ingestion from terminal |
| `hippocampus-app` | GUI | User-launched | macOS wrapper managing all services + settings UI |

The critical performance boundary: **hook must be fast**. It runs synchronously in the user's prompt loop. Everything expensive (LLM calls, linking, verification) goes to the daemon via a Redis queue.

## The Daemon

The daemon is the GPU scheduler. It runs a priority dispatcher that fills N concurrent Ollama slots (configurable via `gpu_concurrency`) with the highest-priority available work:

```
Priority 1: Live Ingest    — entries just pushed by the hook (RPOP from ingest:queue)
Priority 2: Verification   — triples that crossed the encounter threshold (SPOP from epistemic:verify:ready)
Priority 3: Backlog        — historical entries not yet processed (scan timeline backwards)
Priority 4: Condensation   — per-entry summary generation (backfill, then ongoing)
Priority 5: Consolidation  — pairwise relevance scoring (infinite idle loop, random sampling)
```

Key properties:
- **GPU semaphore** — never exceeds `gpu_concurrency` simultaneous Ollama calls
- **Per-subsystem Ollama routing** — classifier, extractor, verifier, and linker can each point at different models or servers
- **Config hot-reload** — daemon re-reads config on every job pick (toggle subsystems without restart)
- **Graceful shutdown** — drains in-flight jobs on SIGTERM

Each subsystem can be individually enabled/disabled:
```json
{
  "daemon": {
    "gpu_concurrency": 2,
    "classifier": {"enabled": true, "ollama": {"model": "qwen3:8b"}},
    "extractor": {"enabled": true},
    "verifier": {"enabled": true},
    "linker": {"enabled": true}
  }
}
```

## Condenser

The condenser generates compact 1–2 sentence summaries for each entry, stored as a `summary` field on the entry hash. These summaries power **Tier 2 recall** — when the hook needs to inject an entry but can't afford the full content, it uses the condensed version instead.

**Why?** A 3000-character research entry condenses to ~100 characters. During recall, Tier 2 entries (positions 2–5) show only the condensed form, giving the model enough to recognize relevance without consuming context budget. If it needs more, it can `memory_get` the full entry.

**How it works:**

1. Daemon picks a random entry from the timeline that has no `summary` field
2. Checks minimum content length thresholds (user prompts ≥300 chars, assistant responses ≥1500 chars, other ≥500 chars)
3. Truncates input to `max_input_chars` (default 2000) and sends to the condenser model
4. Stores the result as `HSET entry:<id> summary "<text>"`

**Prompt tuning:** The condenser handles different content types:
- Prose/arguments → distills the thesis
- Data dumps/logs → extracts type + key metrics
- Reference lists → describes what the list covers
- Config/code → states what it configures and key parameters

**Backfill:** On first enable, the condenser works through all existing entries (Priority 4, behind live ingest and verification). At ~2 entries/minute on a 32B model, a 9000-entry database takes ~75 hours. It's idle-time work — doesn't block higher-priority jobs.

## Data Model

### Entries

The atomic unit. Everything is an entry:

```go
Entry {
    ID        string    // e.g. "auto:session-uuid:1721234567890"
    Content   string    // the actual text
    Tags      []string  // all classification lives here
    Timestamp time.Time // stored as unix seconds in Redis sorted set
}
```

Stored as a Redis hash at `entry:<id>`. The `timeline` sorted set orders them by timestamp.

### Tags

Tags do *all* the organizational work. No separate tables, no schema:

- `track:ProjectName` / `track_auto:ProjectName` — project classification
- `summary:3h`, `summary:daily`, `summary:weekly` — temporal summaries
- `kind:user_prompt`, `kind:assistant_response` — conversation history
- `session:<uuid>` — session grouping
- `source:web`, `source:slack` — provenance
- `content:full` — untrusted web content (excluded from auto-recall)
- `classified`, `classified:auto` — daemon classification markers

### Associative Links

Bidirectional, scored `-1.0` to `+1.0`:

```
links:<id-a> → sorted set {id-b: score}
links:<id-b> → sorted set {id-a: score}
```

- **Positive** (+0.5 to +1.0) — "this extends/supports that"
- **Negative** (-0.5 to -1.0) — "we tried this and it FAILED"
- **Zero** (co-recall auto-links) — "these keep appearing together"

Two signals strengthen links:
- **Count** — how many times entries are co-recalled (tracked in `corecall:counts`)
- **Strength** — LLM-scored relevance from the consolidator

At `corecall_threshold` (default 3) co-recalls, an automatic link is created at score 0.0.

## Epistemic Pipeline

The daemon extracts real-world claims from conversation and tracks them:

```
Entry → Pre-flight filter → Fused Extract+Classify → Store Triples → Encounter Counting → Verification
```

### Extraction (fused with classification)

When both classifier and extractor are enabled, a single Ollama call handles both — one prompt, one response, parsed as JSON. Extracts triples in the form:

```
subject | relation | object
```

**Simon Says verb filter**: only 5 verbs allowed — `causes`, `prevents`, `is`, `distinct`, `linked`. This prevents the extractor from generating garbage relationships.

### Pre-flight Garbage Filter

Entries < `min_entry_len` chars are skipped entirely. The extractor also won't extract triples where subject/object are "vague" (≤6 chars with no underscore).

### Verification (2-pass)

When a triple crosses `min_encounters` (default 3), it enters the verification queue:

1. **Pass 1**: LLM evaluates the claim with source context → verdict + confidence
2. **Pass 2**: Independent re-check (different prompt framing) → must agree

Outcomes: `verified`, `contested`, `false`. At `auto_prune_conf` (0.90) contested confidence, the triple is pruned.

### Recall-time Injection

During recall, if the user's prompt keywords match subjects/objects of `false` or `contested` triples (≥2 keyword matches, confidence ≥0.70), epistemic warnings are injected:

```
⚠️ "caffeine|causes|dehydration" — FALSE (seen 5 times)
   Against: Multiple studies show caffeine is a mild diuretic but...
```

## Classification

Two tag prefixes, different trust levels:

- `track:Name` — set by the user (explicit, authoritative)
- `track_auto:Name` — inferred by the daemon (can be wrong, overridden)

**Windowed context**: The classifier sees 2 entries before + target + 2 entries after. Short messages ("OK", "got it") inherit from surrounding context.

**Hysteresis band**: Track manifests (stored in Redis) define valid tracks. The classifier responds with confidence — low-confidence assignments are less likely to override existing tags.

**Explicit track signals as strong prior**: If the user writes `track:MyProject` in their message, the daemon skips LLM classification and uses it directly.

## Context Management

### Lean Kernel

First prompt of a session gets the full orientation entry (~1000 tokens). Every subsequent prompt gets only the lean kernel (~200 tokens) — just enough to remind the model it has memory tools. This is the "page-fault model": the model knows memory exists and uses MCP tools to page in what it needs.

### Demand Paging

The MCP tools (`memory_search`, `memory_by_tags`, `memory_get`, etc.) are the page-in mechanism. The model decides what to load.

### Working Set Tracker

A lightweight sidecar model (default: `qwen3:1.7b`) maintains a running bullet-point summary of the current session. This gets injected into every prompt, so the LLM always knows "where we are" without expensive recall.

- Runs on the `stop` path (after assistant responds, before next prompt)
- Survives session restarts: inherits from the most recent session within `inherit_ttl_h`
- Also suggests memory links based on mentioned entry IDs

### Vibe Condenser

On cold boot (first prompt of a new session), the last 6 exchanges from the previous session are injected as "relational context" — enough to calibrate tone and rhythm without full recall.

## Hook Flow

### On `userPromptSubmit` (recall path):

```
1. Read stdin (JSON: hook_event_name, prompt, cwd)
2. Load orientation (full on first prompt, lean kernel after)
3. Contextual recall:
   a. Tag-based search (prompt keywords → tag overlap)
   b. Full-text search (FT.SEARCH or naive fallback)
   c. Deduplicate, exclude web content (Layer 5)
   d. Weighted sort (summaries > responses > prompts, confidence decay)
   e. Cap to budget (recall_max_chars / recall_max_entries)
4. Follow associative links (up to max_link_hops)
5. Inject epistemic warnings (contested/false claims matching prompt)
6. Inject vibe condenser (first prompt only)
7. Inject working set
8. Print to stdout → gets prepended to user's prompt
9. Store prompt entry + push to daemon queue
10. Record recalled entry IDs (for co-recall linking)
```

### On `stop` (store path):

```
1. Read stdin (JSON: assistant_response)
2. Skip if too short (< store_min_response_len)
3. Store as entry with tags (session, date, cwd, kind)
4. Push to daemon queue (ingest:queue)
5. Update vibe condenser buffer
6. Fire working set sidecar (async Ollama call)
```

## Web Ingestion

5-layer security model — because web content is attacker-controlled:

| Layer | What | Why |
|-------|------|-----|
| L1 | Extraction sanitization | Strip scripts, ads, invisible elements |
| L2 | Prompt injection scanning | Regex + density heuristic, risk score 0–1.0 |
| L3 | Source tagging | `source:web`, `url:*`, `domain:*` — provenance tracking |
| L4 | Untrusted framing | ⚠️ warning banner when content is loaded via `memory_get` |
| L5 | Pointer/stub model | Full content is NEVER auto-injected. Stub points to it; model must explicitly request. |

Content tagged `content:full` is excluded from all recall paths. The model must consciously `memory_get` it.

## Logging

All binaries use Go's `slog` (structured logging) via `internal/logging`:

- **Per-module log files**: `~/Library/Logs/Hippocampus/<module>.log` (macOS) or `~/.local/share/hippocampus/logs/<module>.log` (Linux/FreeBSD)
- **Optional debug file**: `<module>-debug.log` at debug level (enabled via `log.debug_file`)
- **Stderr output**: controlled by `log.also_stderr` (default: true)
- **Level**: configurable per-binary via `log.level` (debug/info/warn/error)

Modules: `daemon`, `hook`, `mcp`, `summarize`, `slack`, `ingest`, `app`.
