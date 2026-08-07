# Hippocampus Redis Schema Reference (v2.x)

This document captures the current Redis data model as a reference for v3 spec development.

## Key Namespace Map

| Pattern | Type | Purpose |
|---------|------|---------|
| `entry:<id>` | HASH | Core entry storage (content, tags, timestamp, summary) |
| `tag:<tagname>` | SET | Entry IDs that have this tag |
| `tags:all` | SET | All tag names that exist |
| `timeline` | ZSET | All entry IDs, scored by Unix timestamp |
| `links:<id>` | HASH | Associative links (field=targetID, value="score\|type") |
| `hippocampus:vectors` | VSET | Vector embeddings (FP32, keyed by entry ID) |
| `ingest:queue` | LIST | Live ingest queue for daemon (LPUSH/BRPOP) |
| `meta:slack:backfill:<channelID>` | STRING | Backfill cursor (Slack ts or "done") |
| `meta:track-manifests` | HASH | Track classifier manifests (field=content, JSON map) |
| `daemon:last_processed` | STRING | Unix ts of last daemon activity |
| `daemon:last_entry_id` | STRING | Last entry ID processed by daemon |
| `corecall:<idA>:<idB>` | STRING | Co-recall counter (int, incremented on co-occurrence) |

## Entry HASH Fields

```
entry:<id>
├── id        : string   (entry ID, same as key suffix)
├── content   : string   (the actual text)
├── tags      : string   (comma-separated tag list)
├── timestamp : int64    (Unix seconds)
└── summary   : string   (optional: condensed version from daemon)
```

## Link HASH Fields

```
links:<id>
├── <targetID_1> : "0.7500|extends"     (score|type)
├── <targetID_2> : "0.8500|supports"
├── <targetID_3> : "-0.5000|contradicts"
└── <targetID_N> : "0.0000|corecall"    (unscored, pending evaluation)
```

Link types: `corecall`, `legacy`, `overflow`, `thread`, `cross-track`, `imported`, `extends`, `supports`, `contradicts`, `preceded_by`, `manual`, `discovery`, `temporal`

## Tag Taxonomy

```
track:<Name>           — Manual track assignment
track_auto:<Name>      — Classifier-assigned track
summary:3h:<Name>      — 3-hour summary for track
summary:daily:<Name>   — Daily summary for track
summary:weekly:<Name>  — Weekly summary for track
summary:cross-track    — Cross-track connection summaries
summary:comprehensive  — Full orientation entries
kind:prompt            — Bulk-ingested user prompts
kind:assistantmessage  — Bulk-ingested assistant responses
kind:user_prompt       — Hook-captured user prompts (recent)
kind:assistant_response — Hook-captured assistant responses (recent)
kind:reasoning         — Decision breadcrumbs
auto:captured          — Auto-captured by hook this session
session:<uuid>         — Session scope
date:YYYY-MM-DD        — Temporal
cwd:<path>             — Working directory context
person:<name>          — People-related entries
source:slack           — Slack-ingested entries
source:web             — Web-ingested entries
content:slack          — Content type marker
slack:channel:<id>     — Slack channel ID
slack:channel_name:<n> — Slack channel display name
slack:user:<id>        — Slack user ID
slack:user_name:<n>    — Slack user display name
slack:thread:<ts>      — Thread grouping
safety:flagged         — Safeguard scanner flagged
meta                   — System/orientation entries
orientation            — Orientation entries
lean-kernel            — Lean kernel entry
classified             — Entry has been classified
classified:auto        — Classification was automatic
classified:confused    — Classifier was uncertain
```

## State Diagrams

### Entry Lifecycle

```mermaid
stateDiagram-v2
    [*] --> Captured: hook/slack/MCP stores entry

    Captured --> Queued: LPUSH to ingest queue
    Captured --> Indexed: FT.SEARCH auto-indexes on HSET

    Queued --> Processing: daemon BRPOP
    Processing --> Classified: fused classify+extract
    Processing --> ClassifyFailed: Ollama error, retry later

    Classified --> Linked: temporal neighbors + co-recall
    Linked --> Embedded: vector generated (async)
    Embedded --> Complete: entry fully processed

    Classified --> Summarized: summarizer picks up (3h window)
    Summarized --> RolledUp: daily/weekly rollup consumes

    Complete --> Recalled: hook retrieves via RRF
    Recalled --> CoRecalled: co-recall counter incremented
    CoRecalled --> LinkCreated: threshold crossed, link created

    Complete --> Decayed: confidence decay over time
    Decayed --> Dissolved: link score below min, removed
```

### Link Lifecycle

```mermaid
stateDiagram-v2
    [*] --> Created: temporal/thread/discovery/manual

    Created --> Evaluated: daemon consolidation loop
    Evaluated --> Strengthened: LLM confirms relevance (score up)
    Evaluated --> Weakened: LLM doubts relevance (score down)
    Evaluated --> Dissolved: score below MinScore, HDEL

    Strengthened --> Shortcut: traversal frequency high, promote
    Weakened --> ReEvaluated: next consolidation cycle

    Created --> CoRecall: entries recalled together
    CoRecall --> Promoted: count > threshold → real score assigned
    CoRecall --> Stale: no co-recall in N days → dissolve

    state "Discovery" as disc
    [*] --> disc: random pair evaluation (idle time)
    disc --> Created: LLM says "yes, related"
    disc --> Discarded: LLM says "no"
```

### Recall Flow (Hook)

```mermaid
flowchart TD
    A[User prompt arrives] --> B[Hook fires]
    B --> C{First prompt in session?}
    C -->|Yes| D[Load full orientation]
    C -->|No| E[Load lean kernel]

    D --> F[Semantic search: FT.SEARCH + VSIM]
    E --> F

    F --> G[Channel 1: BM25 results]
    F --> H[Channel 2: VSIM vector results]
    F --> I[Channel 3: Link graph traversal]

    G --> J[RRF Fusion]
    H --> J
    I --> J

    J --> K[Ranked results]
    K --> L{Score > relevance_floor?}
    L -->|Yes| M[Inject into context]
    L -->|No| N[Discard]

    M --> O[Working set injection]
    O --> P[Track orientation injection]
    P --> Q[Neuromodulator state]
    Q --> R[Output to stdout → model context]
```

### Summarization Hierarchy

```mermaid
flowchart BT
    A[Raw entries] -->|3h window| B[summary:3h:Track:YYYY-MM-DD-HH]
    B -->|daily rollup| C[summary:daily:Track:YYYY-MM-DD]
    C -->|weekly rollup| D[summary:weekly:Track:YYYY-MM-DD]
    D -->|cross-track| E[summary:cross-track:YYYY-MM-DD]

    style A fill:#f9f,stroke:#333
    style B fill:#bbf,stroke:#333
    style C fill:#bfb,stroke:#333
    style D fill:#fbf,stroke:#333
    style E fill:#ff9,stroke:#333
```

### Daemon Priority Dispatch

```mermaid
flowchart TD
    A[Daemon starts] --> B{Check ingest:queue}
    B -->|Non-empty| C[BRPOP → jobLiveIngest]
    B -->|Empty| D{Backlog cursor exhausted?}

    D -->|No| E[Scan timeline → jobBacklog]
    D -->|Yes| F{Idle work available?}

    F -->|Yes| G[Discovery link / Re-evaluate / Embed backfill]
    F -->|No| H[Sleep idlePollS]

    C --> I[processEntry]
    E --> I
    I --> J[Classify + Extract + Link + Embed]
    J --> B

    G --> B
    H --> B
```

## v3 Schema Considerations

- **Config versioning:** Store schema version in DB 1 (`schema:version`)
- **Namespace isolation:** Key prefix per tenant (`t:<tenantID>:entry:<id>`)
- **Bi-temporal:** Add `valid_from`/`valid_until` fields for fact evolution
- **Confidence field:** Float on entry HASH, decayed by summarizer
- **Stale tracking:** Tag `stale` on summaries whose sources changed
- **Source provenance:** `sources` field on summary entries (comma-separated IDs consumed)
