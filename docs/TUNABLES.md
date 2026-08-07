# Configuration Reference

All configuration lives in a single JSON file. Location search order:

1. `$HIPPOCAMPUS_CONFIG` (env var)
2. `~/Library/Application Support/Hippocampus/config.json` (macOS)
3. `~/.config/hippocampus/config.json` (Linux/FreeBSD)
4. `./config.json` (CWD fallback)
5. `/etc/hippocampus/config.json` (system-wide)

Every field has a sensible default. A minimal config is just:

```json
{
  "redis": {"addr": "localhost:6379"}
}
```

---

## Top-Level

| Key | Default | Description |
|-----|---------|-------------|
| `author` | `""` | Attribution tag for multi-user setups (e.g. `"alice"`). Empty = no attribution. |

## Redis

Connection to Redis/Valkey.

| Key | Default | Description |
|-----|---------|-------------|
| `redis.addr` | `localhost:6379` | Redis/Valkey host:port |
| `redis.password` | `""` | AUTH password |
| `redis.username` | `""` | ACL username (Valkey 6+) |
| `redis.db` | `0` | Database number |
| `redis.tls` | `false` | Enable TLS |
| `redis.tls_cert` | `""` | Client certificate path |
| `redis.tls_key` | `""` | Client key path |
| `redis.tls_ca` | `""` | CA certificate path |
| `redis.tls_insecure` | `false` | Skip server cert verification |
| `redis.max_retries` | `3` | Connection retry attempts |
| `redis.dial_timeout_s` | `5` | Connection timeout in seconds |
| `redis.pool_size` | `10` | Connection pool size |

## Memory

Controls what gets stored and recalled on each prompt.

| Key | Default | Description |
|-----|---------|-------------|
| `memory.max_search_results` | `20` | MCP search result limit |
| `memory.recall_max_chars` | `8000` | Max total chars injected per prompt |
| `memory.recall_max_entries` | `10` | Max entries injected per prompt |
| `memory.recall_scan_depth` | `100` | Entries scanned in naive fallback (no FT.SEARCH) |
| `memory.store_max_chars` | `0` | Max chars per stored entry (0 = unlimited) |
| `memory.store_min_prompt_len` | `20` | Prompts shorter than this are not stored |
| `memory.store_min_response_len` | `100` | Responses shorter than this are not stored |
| `memory.decay_half_life_days` | `30` | Confidence half-life in days (0 = no decay) |
| `memory.relevance_floor` | `0.05` | Min weight to inject a recalled entry. Raise to filter out weak matches. |
| `memory.global_summary_max_chars` | `4000` | Max chars for global track summary injection |
| `memory.summarize_max_entries` | `200` | Max entries per track for summarization |
| `memory.summarize_max_input_chars` | `50000` | Max chars fed to LLM for summarization |
| `memory.classify_max_chars` | `500` | Max chars per entry for classification window |
| `memory.cross_track_max_chars` | `3000` | Max chars per summary for cross-track analysis |

## Ollama

Global LLM settings. Subsystems inherit from here unless overridden.

| Key | Default | Description |
|-----|---------|-------------|
| `ollama.base_url` | `http://localhost:11434` | Ollama API endpoint |
| `ollama.model` | `qwen3:32b` | Default model (summarization, extraction) |
| `ollama.timeout_minutes` | `10` | HTTP timeout for generation |
| `ollama.wedge_timeout_seconds` | `90` | Seconds without a token before declaring model wedged. Lower if you want faster recovery from stuck generations. |
| `ollama.max_retries` | `2` | Retries per generation after wedge or failure |
| `ollama.embedding_model` | `nomic-embed-text` | Model for vector embeddings |
| `ollama.embedding_dimensions` | `768` | Vector dimensions |

## Daemon

The async priority dispatcher. Handles classification, extraction, verification, and linking.

| Key | Default | Description |
|-----|---------|-------------|
| `daemon.enabled` | `false` | Enable the daemon (macOS app enables automatically) |
| `daemon.gpu_concurrency` | `2` | Max simultaneous Ollama calls |
| `daemon.queue_key` | `"ingest:queue"` | Redis list key for live ingest |
| `daemon.backlog_batch` | `50` | Entries per backlog scan cycle |
| `daemon.backlog_pause_s` | `5` | Pause between backlog entries (seconds) |
| `daemon.corecall_threshold` | `3` | Co-recall count to auto-create link |
| `daemon.recalled_ttl_h` | `24` | TTL for recalled entry sets (hours) |
| `daemon.idle_poll_s` | `2` | Seconds between polling when queue is empty. Lower = more responsive, higher = less CPU. |
| `daemon.self_update_check_s` | `30` | How often to check binary mtime for hot-reload (seconds) |
| `daemon.failure_threshold` | `3` | Consecutive Ollama failures before triggering exponential backoff |
| `daemon.max_backoff_s` | `3600` | Maximum backoff cap in seconds (1 hour ceiling) |
| `daemon.backoff_base_s` | `10` | Base duration for exponential backoff (doubles on each failure) |

### GPU Concurrency

`gpu_concurrency` controls how many Ollama generation requests run in parallel. This is a semaphore — the daemon blocks acquiring a slot before dispatching any job.

Set this based on your GPU VRAM and model sizes:
- **1** — safe for single GPU with a large model (32B+)
- **2** — typical for 24GB+ VRAM with mixed model sizes
- **4+** — multi-GPU or small models only

The daemon never exceeds this limit regardless of queue depth.

### Subsystem Configuration

Each subsystem (classifier, extractor, verifier, linker) can be independently enabled and routed to a different Ollama endpoint/model:

| Key | Default | Description |
|-----|---------|-------------|
| `daemon.classifier.enabled` | `true` | Run track classification |
| `daemon.classifier.ollama.base_url` | *(inherit)* | Override Ollama endpoint |
| `daemon.classifier.ollama.model` | *(inherit)* | Override model |
| `daemon.extractor.enabled` | `true` | Run epistemic extraction |
| `daemon.extractor.ollama.base_url` | *(inherit)* | Override Ollama endpoint |
| `daemon.extractor.ollama.model` | *(inherit)* | Override model |
| `daemon.verifier.enabled` | `true` | Run 2-pass verification |
| `daemon.verifier.ollama.base_url` | *(inherit)* | Override Ollama endpoint |
| `daemon.verifier.ollama.model` | *(inherit)* | Override model |
| `daemon.linker.enabled` | `true` | Run co-recall + temporal linking |
| `daemon.linker.ollama.base_url` | *(inherit)* | Override Ollama endpoint |
| `daemon.linker.ollama.model` | *(inherit)* | Override model |

Resolution order: subsystem override → `daemon.ollama` override → global `ollama.*`.

### Condenser (per-entry condensation)

Generates condensed summaries for individual entries. Runs as a daemon subsystem.

| Key | Default | Description |
|-----|---------|-------------|
| `daemon.condenser.enabled` | `true` | Enable/disable per-entry condensation |
| `daemon.condenser.min_user_chars` | `300` | User prompt must be longer than this to trigger condensation |
| `daemon.condenser.min_assistant_chars` | `1500` | Assistant response must be longer than this to trigger |
| `daemon.condenser.min_other_chars` | `500` | Other entry types (e.g. ingested content) threshold |
| `daemon.condenser.max_output_chars` | `250` | Max length of the condensed summary |
| `daemon.condenser.max_input_chars` | `2000` | Input text truncated to this before sending to LLM |
| `daemon.condenser.ollama.base_url` | *(inherit)* | Override Ollama endpoint for condensation |
| `daemon.condenser.ollama.model` | *(inherit)* | Override model for condensation |

### TSA (RFC 3161 Timestamping)

Cryptographic integrity verification for memory entries. The daemon periodically computes content hashes for entries and submits Merkle block roots to an RFC 3161 Time Stamp Authority. On recall, the MCP server verifies entry integrity against stored hashes.

| Key | Default | Description |
|-----|---------|-------------|
| `daemon.tsa.enabled` | `false` | Enable/disable TSA timestamping |
| `daemon.tsa.url` | `"https://rfc3161.ai.moda"` | RFC 3161 TSA endpoint URL |
| `daemon.tsa.batch_size` | `256` | Entries per Merkle block |
| `daemon.tsa.interval_s` | `3600` | Seconds between TSA runs (default: hourly) |
| `daemon.tsa.hash_algo` | `"sha256"` | Hash algorithm (only sha256 currently supported) |

**How it works:**

1. **Hash backfill** — Each cycle, entries without a `content_hash` field get one computed: `SHA-256(id + "\n" + content + "\n" + timestamp)`. Skips `meta:*` and `summary:*` entries.

2. **Merkle stamp** — Up to `batch_size` unhashed entries are collected into a sorted Merkle tree. The root is submitted to the TSA via RFC 3161. The signed timestamp response (TSR) is stored as a `tsa:<block_id>` entry with the full proof data.

3. **Recall-time verification** — When the MCP server returns entries (via `memory_get`, `memory_search`, `memory_by_tags`, `memory_recent`, `memory_time_range`, `memory_session_context`), it recomputes the hash and compares to the stored `content_hash`. Entries gain an `integrity` field:
   - `"verified"` — hash matches stored content_hash
   - `"unattested"` — no content_hash yet (candidate for backfill)
   - `"FAILED"` — hash mismatch (possible tampering)

**Startup delay:** The TSA loop waits 60 seconds after daemon start before its first cycle, allowing other subsystems to settle.

### Daemon-level Ollama Override

Applies to all subsystems that don't have their own override:

| Key | Default | Description |
|-----|---------|-------------|
| `daemon.ollama.base_url` | *(inherit from global)* | Daemon-wide endpoint |
| `daemon.ollama.model` | *(inherit from global)* | Daemon-wide model |

## Epistemic

The fact-checking / knowledge extraction pipeline.

### Extraction Settings

| Key | Default | Description |
|-----|---------|-------------|
| `epistemic.max_text_len` | `3000` | Max chars of source text sent to extractor |
| `epistemic.max_vocab_terms` | `50` | Vocabulary terms for entity reconciliation |
| `epistemic.min_entry_len` | `50` | Skip entries shorter than this |
| `epistemic.max_keywords` | `30` | Keyword cap for vocabulary lookup |
| `epistemic.min_keyword_len` | `4` | Min keyword length to consider |

### Verification Settings

| Key | Default | Description |
|-----|---------|-------------|
| `epistemic.min_encounters` | `3` | Encounters before triggering verification |
| `epistemic.max_verify_batch` | `20` | Max triples verified per run |
| `epistemic.auto_prune_conf` | `0.90` | Contested confidence ≥ this → auto-prune |
| `epistemic.reinforce_boost` | `0.05` | Confidence boost on re-verification agreement |
| `epistemic.source_context_max` | `500` | Max chars per source entry in verification prompt |
| `epistemic.max_source_entries` | `5` | Max source entries gathered for context |

### Recall-time Warning Injection

| Key | Default | Description |
|-----|---------|-------------|
| `epistemic.warning_conf_min` | `0.70` | Min confidence to inject warning |
| `epistemic.warning_min_keys` | `2` | Require ≥ N keyword matches to trigger |
| `epistemic.max_warnings` | `5` | Max warnings injected per prompt |
| `epistemic.evidence_trunc` | `80` | Truncate evidence text at N chars |

### Structural Filters

| Key | Default | Description |
|-----|---------|-------------|
| `epistemic.vague_max_len` | `6` | Terms ≤ this length (no underscore) = "vague", skipped |

## Ingest

Web page ingestion security and chunking.

| Key | Default | Description |
|-----|---------|-------------|
| `ingest.reject_threshold` | `0.8` | Risk score ≥ this → reject page |
| `ingest.sanitize_threshold` | `0.5` | Risk score ≥ this → sanitize |
| `ingest.safety_threshold` | `0.5` | Below this = considered safe |
| `ingest.density_ratio` | `0.3` | Instruction sentence ratio trigger |
| `ingest.density_min_count` | `3` | Min instruction-like sentences to trigger |
| `ingest.web_content_weight` | `0.3` | Recall weight for full web content |
| `ingest.stub_weight` | `0.6` | Recall weight for stub/pointer entries |
| `ingest.fetch_timeout_s` | `30` | HTTP fetch timeout (seconds) |
| `ingest.max_body_bytes` | `10485760` | Max HTML download size (10MB) |
| `ingest.user_agent` | `""` | Custom User-Agent header (empty = default browser UA) |
| `ingest.max_chunk_size` | `3000` | Max chars per chunk |
| `ingest.min_chunk_size` | `200` | Min chunk size before merging |

## Consolidation

Background pairwise relevance discovery. Runs as Priority 4 in the daemon, or via `hippocampus-summarize --consolidate`.

| Key | Default | Description |
|-----|---------|-------------|
| `consolidation.pairs_per_run` | `5` | Pairs evaluated per cycle |
| `consolidation.min_score` | `0.4` | Min |score| to create a link |
| `consolidation.drift_delta` | `0.2` | Score change threshold to update existing link |
| `consolidation.cycle_pause_s` | `600` | Seconds between cycles |
| `consolidation.max_entries` | `500` | Max entries sampled per run |
| `consolidation.content_truncation` | `500` | Chars per entry in LLM prompt |
| `consolidation.min_content_length` | `50` | Min content length to be linkable |
| `consolidation.temperature` | `0.1` | LLM temperature for scoring |
| `consolidation.max_tokens` | `200` | Max LLM response tokens |
| `consolidation.eval_timeout_s` | `60` | Per-pair LLM eval timeout (seconds) |
| `consolidation.cooldown_ttl_s` | `3600` | Seconds before a pair can be re-evaluated |
| `consolidation.discovery_min_len` | `200` | Min content chars for random discovery sampling |

## Hook

Settings for the sync hook binary (recall + store path).

| Key | Default | Description |
|-----|---------|-------------|
| `hook.timeout_s` | `10` | Redis operation timeout |
| `hook.boot_phase_ttl_h` | `24` | Hours before re-injecting full orientation |
| `hook.max_link_hops` | `3` | Associative link traversal depth |
| `hook.hook_timeout_ms` | `10000` | Timeout written to generated agent configs |
| `hook.link_budget_chars` | `3000` | Max chars for linked recall results (controls how much linked content is injected) |
| `hook.link_budget_entries` | `3` | Max entries followed via associative links |
| `hook.min_link_follow_score` | `0.3` | Min |score| to follow a link during recall. Raise to only follow strong links. |
| `hook.tier2_max_chars` | `300` | Condensed content length for tier 2 recall (medium-relevance entries) |
| `hook.tier3_snippet_chars` | `80` | Breadcrumb snippet length for tier 3 recall (low-relevance entries) |
| `hook.vibe_max_exchanges` | `6` | Max vibe exchanges stored per session |
| `hook.vibe_truncate_chars` | `200` | Vibe text truncation per entry |

## MCP

MCP server defaults (applied when client doesn't specify limits).

| Key | Default | Description |
|-----|---------|-------------|
| `mcp.default_search_limit` | `10` | Default `memory_search` results |
| `mcp.default_tag_limit` | `20` | Default `memory_by_tags` results |
| `mcp.default_time_range_limit` | `20` | Default `memory_time_range` results |

## Working Set

Sidecar model that maintains a session context summary.

| Key | Default | Description |
|-----|---------|-------------|
| `working_set.enabled` | `false` | Enable the working set tracker |
| `working_set.model` | `qwen3:1.7b` | Ollama model for context tracking |
| `working_set.max_bullets` | `5` | Max bullet points in summary |
| `working_set.max_chars` | `500` | Max chars in working set |
| `working_set.timeout_s` | `120` | Timeout for sidecar LLM call in seconds |
| `working_set.inherit_ttl_h` | `24` | Inherit from previous session within this window |

## Slack

Slack bot configuration.

| Key | Default | Description |
|-----|---------|-------------|
| `slack.bot_token` | `""` | Bot User OAuth Token (`xoxb-...`) |
| `slack.app_token` | `""` | App-Level Token (`xapp-...`) for Socket Mode |
| `slack.channels` | `[]` | Channels to monitor (array of objects) |

Channel objects:
```json
{
  "id": "C0123ABC",
  "name": "#engineering",
  "mode": "archive",
  "tags": ["source:slack", "team:eng"],
  "backfill": false
}
```

- `mode`: `"archive"` (silent, default) or `"active"` (responds to mentions)
- `tags`: auto-applied to all messages from this channel
- `backfill`: incrementally ingest channel history

## Log

Per-binary structured logging via Go's `slog`.

| Key | Default | Description |
|-----|---------|-------------|
| `log.log_dir` | *(auto)* | Log directory. macOS: `~/Library/Logs/Hippocampus/`, Linux: `~/.local/share/hippocampus/logs/` |
| `log.level` | `"info"` | Min level: `debug`, `info`, `warn`, `error` |
| `log.debug_file` | `false` | Write a separate `<module>-debug.log` at debug level |
| `log.also_stderr` | `true` | Also write to stderr |

Each binary writes to `<log_dir>/<module>.log` (e.g. `daemon.log`, `hook.log`, `mcp.log`).

## OFC (Orbitofrontal Cortex)

Experimental neuromodulator system. Tracks dopamine/serotonin analogs based on user sentiment.

| Key | Default | Description |
|-----|---------|-------------|
| `ofc.enabled` | `false` | Enable OFC module (also via `--ofc` flag on hook) |
| `ofc.model` | `qwen3:8b` | Model for sentiment analysis (empty = regex fallback) |
| `ofc.classify_timeout_s` | `3` | OFC model timeout in seconds. Keep low since it runs on the hot path. |
| `ofc.da_decay` | `0.95` | Dopamine decay per prompt (toward 0) |
| `ofc.sht_decay` | `0.98` | Serotonin decay per prompt (toward baseline) |
| `ofc.sht_baseline` | `0.5` | Serotonin neutral baseline |
| `ofc.da_explicit_positive` | `0.08` | DA bump on explicit positive signal |
| `ofc.da_explicit_negative` | `-0.12` | DA hit on explicit negative signal |
| `ofc.da_implicit_positive` | `0.03` | DA bump on implicit positive |
| `ofc.da_implicit_negative` | `-0.05` | DA hit on implicit negative |
| `ofc.sht_positive` | `0.03` | 5HT bump on positive signal |
| `ofc.sht_negative` | `-0.04` | 5HT hit on negative signal |

## Environment Variables

| Variable | Description |
|----------|-------------|
| `HIPPOCAMPUS_CONFIG` | Path to config file (overrides default search order) |
| `KIRO_SESSION_ID` | Session identifier (set by the calling AI client) |

---

## Example: Minimal Config

```json
{
  "redis": {"addr": "localhost:6379"}
}
```

Everything else uses defaults. Good enough for local development.

## Example: Sample Production Config

```json
{
  "redis": {
    "addr": "redis.local:6379",
    "password": "hunter2",
    "tls": true,
    "pool_size": 20
  },
  "memory": {
    "recall_max_chars": 12000,
    "recall_max_entries": 15,
    "decay_half_life_days": 60
  },
  "ollama": {
    "base_url": "http://gpu-box:11434",
    "model": "qwen3:32b",
    "embedding_model": "nomic-embed-text"
  },
  "daemon": {
    "enabled": true,
    "gpu_concurrency": 4,
    "classifier": {"enabled": true, "ollama": {"model": "qwen3:8b"}},
    "extractor": {"enabled": true},
    "verifier": {"enabled": true},
    "linker": {"enabled": true},
    "condenser": {"enabled": true, "ollama": {"model": "qwen3:1.7b"}}
  },
  "working_set": {
    "enabled": true,
    "model": "qwen3:1.7b",
    "max_bullets": 7
  },
  "log": {
    "level": "debug",
    "debug_file": true
  },
  "slack": {
    "bot_token": "xoxb-...",
    "app_token": "xapp-...",
    "channels": [
      {"id": "C0123ABC", "name": "#engineering", "mode": "archive", "tags": ["team:eng"]}
    ]
  },
  "author": "alice"
}
```
