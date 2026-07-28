# Hippocampus Architecture

This document covers the current v2.x architecture and the planned v3 architecture.
View with VSCode Markdown Preview (Cmd+Shift+V) for rendered Mermaid diagrams.

---

## v2.x — Current Architecture

### Component Overview

```mermaid
graph TB
    subgraph Client["Client (macOS)"]
        APP[Hippocampus.app\nWails GUI]
        HOOK[hippocampus-hook\nper-message recall/store]
        KIRO[Kiro CLI]
    end

    subgraph Binaries["Background Binaries"]
        DAEMON[hippocampus-daemon\nclassify · link · condense]
        SUMM[hippocampus-summarize\n3h → daily → weekly → global]
        MCP[hippocampus-mcp\nMCP server]
        ADMIN[hippocampus-admin\nCLI maintenance]
    end

    subgraph Store["Redis / Valkey"]
        DB0[(DB 0\nAll entries)]
    end

    subgraph LLM["Local LLM"]
        OLLAMA[Ollama\nqwen3:32b · nomic-embed]
    end

    KIRO -->|stdin/stdout hook| HOOK
    HOOK -->|recall + store| DB0
    HOOK -->|MCP tools| MCP
    MCP -->|read/write| DB0
    APP -->|settings + status| DB0
    DAEMON -->|classify · link · condense| DB0
    DAEMON -->|LLM calls| OLLAMA
    SUMM -->|read entries\nwrite summaries| DB0
    SUMM -->|LLM calls| OLLAMA
    ADMIN -->|read/write| DB0
```

### Retrieval Pipeline (Hook Recall)

```mermaid
flowchart LR
    Q[User Prompt] --> KW[Extract Keywords]
    KW --> TAG["Tag Search\nFT.SEARCH"]
    KW --> FTS["Full-Text Search\nBM25"]
    TAG --> SEEDS[Seed Entries]
    FTS --> SEEDS
    SEEDS --> GRAPH["Graph Traversal\n2-hop, decay, abs-score"]
    TAG --> RRF
    FTS --> RRF
    GRAPH --> RRF["RRF Fusion\nsum 1/60+rank"]
    RRF --> FILTER["Relevance Floor\n+ Budget Cap"]
    FILTER --> TIER["Tiered Injection\n20% full, 30% condensed, 50% breadcrumb"]
    TIER --> CTX[Agent Context]
```

### Daemon Priority Dispatcher

```mermaid
flowchart TB
    P1[Priority 1: Live Ingest\nRPOP ingest:queue] --> P2
    P2[Priority 2: Verification\nSPOP epistemic:verify:ready] --> P3
    P3[Priority 3: Backlog\ncursor scan newest→oldest] --> P4
    P4[Priority 4: Condense\npick uncondensed entry] --> P5
    P5[Priority 5: Consolidation\nidle loop — link · discover · reinforce]

    style P1 fill:#d32f2f,color:#fff
    style P2 fill:#f57c00,color:#fff
    style P3 fill:#fbc02d,color:#000
    style P4 fill:#388e3c,color:#fff
    style P5 fill:#1565c0,color:#fff
```

### Memory Entry Lifecycle

```mermaid
stateDiagram-v2
    [*] --> Raw: hook stores entry
    Raw --> Classified: daemon classifies\ntrack + triples
    Classified --> Condensed: daemon condenses\nsummary field
    Condensed --> Linked: consolidation loop\nco-recall · discovery · Hebbian
    Linked --> Summarized: summarizer\n3h block
    Summarized --> DailyRollup: summarizer\ndaily rollup
    DailyRollup --> WeeklyRollup: summarizer\nweekly rollup
    WeeklyRollup --> Global: summarizer\nglobal summary
```

---

## v3 — Planned Architecture

### Multi-Tenant BLP Design

```mermaid
graph TB
    subgraph Redis["Valkey / Redis (standalone)"]
        DB0[(DB 0\nPublic\neveryone reads)]
        DB1[(DB 1\nHaaS Internal\nserver only)]
        DB2[(DB 2\nTenant A\nprivate + copy of DB 0)]
        DB3[(DB 3\nTenant B\nprivate + copy of DB 0)]
    end

    subgraph Airlift["Epistemic Airlift (one-way)"]
        REPL[public-replicator.so]
    end

    subgraph Auth["Auth Layer"]
        API[Hippocampus API Server\nBLP enforcement]
        CFG[auth tokens\nbilling state]
    end

    DB0 -->|one-way only| REPL
    REPL --> DB2
    REPL --> DB3
    DB1 --- CFG
    API -->|reads auth from| DB1
    API -->|scoped access| DB2
    API -->|scoped access| DB3

    style DB0 fill:#1565c0,color:#fff
    style DB1 fill:#b71c1c,color:#fff
    style DB2 fill:#2e7d32,color:#fff
    style DB3 fill:#4a148c,color:#fff
```

**BLP invariant (structural, not policy):**
- DB 0 → DB N: ✅ allowed (airlift)
- DB N → DB 0: ❌ impossible (no write path)
- DB N → DB M: ❌ impossible (no cross-tenant path)

### Plugin Architecture

```mermaid
graph LR
    subgraph Daemon["hippocampus-daemon"]
        CORE[Priority Dispatcher]
        CORE --> CL[classifier.so]
        CORE --> VE[verifier.so]
        CORE --> LK[linker.so]
        CORE --> CD[condenser.so]
    end

    subgraph CloudPlugins["Cloud-only plugins"]
        CORE --> RD[redactor.so\nBLP enforcement]
        CORE --> AE[auth-enforcer.so]
        CORE --> PR[public-replicator.so]
        CORE --> MT[metering.so\nAWS Marketplace API]
    end

    style CloudPlugins fill:#fafafa,stroke:#999,stroke-dasharray:5
```

### MCP Backend Modes

```mermaid
flowchart LR
    AGENT[AI Agent] --> MCP[hippocampus-mcp]
    MCP -->|backend: redis| REDIS[(Direct Redis\nopen source / homelab)]
    MCP -->|backend: api| HAPI[Hippocampus API\ncloud product]
    HAPI -->|BLP scoped| DB2[(Tenant DB)]
    HAPI -->|auth from| DB1[(Admin DB)]
```

### Complete Hybrid Inference Pipeline (VBrain integration)

```mermaid
flowchart TB
    USER[User Query]
    USER --> HIP

    subgraph HIP["Hippocampus (episodic memory)"]
        RECALL[RRF Retrieval\nVSIM · FT · graph]
        ANTI[Anti-memory\nnegative links]
        RECALL --> CTX[Context + Anti-memories]
        ANTI --> CTX
    end

    CTX --> VBRAIN

    subgraph VBRAIN["VBrain (calibrated cortex)"]
        SPATIAL[Voronoi spatial net\nE/I/A cell types]
        NEURO[Neuromodulators\nDA · NE · 5HT]
        MYELIN[Hebbian myelination\ncritical period]
        SPATIAL --- NEURO
        SPATIAL --- MYELIN
        CONF[Calibrated confidence\nemergent uncertainty]
    end

    VBRAIN --> GATE{Confidence\ngate}
    GATE -->|high confidence| DIRECT[Direct answer]
    GATE -->|low confidence| LLM[Large Language Model\nClaude · GPT · etc.]
    LLM --> OUT[Output + uncertainty annotation]
    DIRECT --> OUT
```

### AWS Marketplace AMI

```mermaid
graph TB
    subgraph AWS["Customer AWS VPC"]
        AMI[EC2 AMI\nhippocampus-daemon\n+ Valkey + plugins]
        ALB[ALB / stunnel\nTLS endpoint]
        BED[Amazon Bedrock\ncloud LLM inference]
        METER[AWS Marketplace\nMetering API]
    end

    subgraph Agents["Developer Machines"]
        A1[Agent 1\nKiro / Claude]
        A2[Agent 2\nCursor]
        A3[Agent N]
    end

    A1 -->|MCP| ALB
    A2 -->|MCP| ALB
    A3 -->|MCP| ALB
    ALB --> AMI
    AMI --> BED
    AMI -->|$0.001/entry\n$2/user/mo| METER

    style AWS fill:#f0f4ff,stroke:#3949ab
    style Agents fill:#f9fbe7,stroke:#558b2f
```

---

## Key Design Principles

| Principle | Implementation |
|---|---|
| No magic numbers | All tunables in config.json with defaults |
| Boring technology | Single Valkey instance, no vector DB |
| Structural not policy | BLP via DB numbers, not query filters |
| Additive only | Graph channel never degrades recall |
| Biological inspiration | Sleep consolidation, spreading activation, Hebbian learning |
| One-way information flow | Public → tenant airlift, never reversed |
