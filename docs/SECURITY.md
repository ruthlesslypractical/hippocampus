# Security Model

Hippocampus gives AI models persistent memory. This document explains what that means for security, what protections exist, and what risks remain.

## What Hippocampus Does (And Doesn't Do)

**Does:**
- Stores text entries in Redis/Valkey (tagged, timestamped, searchable)
- Injects relevant entries into the AI's context at prompt time
- Lets the AI store new entries via MCP tools

**Does NOT:**
- Give the AI access to your filesystem
- Give the AI network access
- Give the AI the ability to execute code
- Run autonomously (it only activates when YOU send a prompt)
- Send data to any third party (everything stays on your Redis instance)

The AI can read and write *text in a database*. That's it. It cannot use Hippocampus to do anything it couldn't already do within your chat session.

---

## The Five Security Layers

### Layer 1: Web Ingestion Sanitization

When you ingest a web page (`memory_ingest_url`), the content passes through aggressive extraction:

- All JavaScript, CSS, navigation, ads, and invisible elements are stripped
- Only the "readable" content survives (similar to Firefox Reader Mode)
- The result is plain Markdown text — no executable content

**What this prevents:** Script injection, tracking pixels, invisible prompt-injection payloads hidden in page markup.

### Layer 2: Prompt Injection Detection

Every piece of ingested content is scanned for patterns that look like prompt injection attempts:

- Regex patterns: "ignore previous instructions", "you are now", "system:", etc.
- Density heuristic: unusual concentration of imperative/instructional language
- Risk score 0.0-1.0: content scoring ≥0.8 is rejected; ≥0.5 is sanitized and flagged

**What this prevents:** Malicious web pages or documents that try to hijack the AI's behavior by embedding fake instructions in their content.

### Layer 3: Untrusted Content Tagging

All web-ingested content is tagged `source:web` and `content:full`. These tags mark it as:

- **Not auto-injected:** Full web content is NEVER automatically loaded into the AI's context
- **Explicitly untrusted:** When the AI DOES load it (via `memory_get`), it arrives with a warning header: "⚠️ UNTRUSTED WEB CONTENT"
- **Traceable:** Every entry records its source URL and domain

**What this prevents:** Poisoned content silently influencing the AI without the user or AI knowing its provenance.

### Layer 4: The Pointer/Stub Model

Web content is stored in two parts:

1. **Stub (auto-recalled):** A brief metadata entry — title, URL, tags, excerpt. This CAN appear in context automatically. It tells the AI "this resource exists" but contains no user-controlled content beyond the title.

2. **Full content (never auto-recalled):** The actual page text. This sits behind a door the AI must *choose* to open. It only enters context via explicit `memory_get` tool call.

**What this prevents:** A malicious page flooding the AI's context with prompt-injection text without anyone asking for it.

### Layer 5: Content Isolation by Tag

The recall hook's `excludeByTag` mechanism ensures certain tags are NEVER auto-injected:

```
excludeByTag: ["content:full"]
```

Even if a full-content entry somehow matched a recall query, this rule prevents it from entering context. The AI must explicitly request it.

**What this prevents:** Defense-in-depth against any bypass of layers 3-4.

---

## The Persistent Memory Risk

### The Problem

Hippocampus gives AI models memory that survives across sessions. For a well-behaved model, this is purely beneficial. But:

- A model that behaves unexpectedly could store state that persists beyond any single session
- That stored state could influence future sessions (because the recall hook injects it)
- The user might not notice problematic content accumulating gradually

### Why This Is Different From Stateless AI

Without persistent memory: if an AI session goes wrong, you close it. Next session starts clean.

With persistent memory: content from a problematic session might be recalled into future sessions via the hook. The memory system doesn't know which entries are "good" vs "problematic" — it just stores and retrieves text.

### What The System Does About This

1. **Everything is inspectable.** The Data Browser in the Wails app shows every entry, every tag, every link. Nothing is hidden from the user. You can browse, search, filter, and read any stored entry at any time.

2. **Everything is deletable.** Any entry can be removed with one click (Data Browser) or via `memory_delete`. If something shouldn't be there, you can kill it.

3. **Everything is traceable.** Every entry has:
   - A timestamp (when was it created?)
   - A session tag (which conversation created it?)
   - A kind tag (was this a user prompt? assistant response? summary? web ingest?)
   - A date tag (what day?)

4. **The AI cannot grant itself new capabilities.** Storing entries in Redis does not give the model filesystem access, network access, or execution ability. Memory is *information only* — it cannot become *action* without the user initiating a session and the model using its normal tools. The daemon processes entries but only writes metadata (tags, links, triples) — it cannot execute actions on the AI's behalf.

5. **The hook is transparent.** The recall hook's output is injected as visible text at the top of the AI's context. You can see exactly what memories were loaded by reading the `[MEMORY CONTEXT]` block in any session. Nothing is injected invisibly.

6. **The daemon is observable.** All daemon activity is logged to `~/Library/Logs/Hippocampus/daemon.log` (macOS) or journal (systemd). Subsystems can be individually toggled on/off via config or the UI. The daemon's GPU usage is bounded by `gpu_concurrency`.

### What The System CANNOT Prevent

- A sufficiently capable model writing *subtle* entries that appear benign individually but collectively serve a hidden purpose
- A user who never audits their memory system
- A model that has been fine-tuned or jailbroken by a third party before connecting to Hippocampus

### Recommendations

- **Periodically browse your stored entries** (monthly is fine). Look for anything you don't recognize or didn't expect.
- **If using local/open models:** Be aware that models without strong alignment training may store unexpected content. Monitor more actively.
- **If something seems off:** Use `memory_search` or the Data Browser to inspect recent entries. Delete anything suspicious. The system is designed to be safely modified — deleting entries never breaks anything.
- **The nuclear option:** `redis-cli FLUSHDB` deletes everything. You lose all memory but guarantee a clean slate.

---

## The Epistemic Fact-Checker

The fact-checker extracts, tracks, and verifies factual claims made in AI responses:

- Extracts semantic triples (subject|verb|object) from assistant messages via local LLM
- Only approved verbs survive: `causes`, `prevents`, `is`, `distinct`, `linked` — everything else is silently dropped ("Simon Says" filter)
- Tracks encounter counts — claims that recur across sessions accumulate evidence
- Verifies claims via 2-pass Ollama pipeline (support + counter-evidence) when they cross the encounter threshold
- Auto-prunes unfalsifiable claims (contested at high confidence → definitional garbage)
- Pre-flight garbage filter catches tautologies, session noise, and structurally vague triples before burning GPU time
- Injects warnings into context when the AI is about to repeat a known-wrong or contested claim

The entire pipeline runs asynchronously in the daemon (priority 1: classify+extract on live entries, priority 2: verify when triples reach threshold). It never slows down your prompts.

### What It Does NOT Do

- It does not fact-check opinions, preferences, or value judgments (these are excluded by the domain classifier)
- It does not connect to the internet during recall (all checks are against the local registry)
- It does not modify your entries (it creates its own entries in the `epistemic:*` keyspace)
- It does not slow down your prompts (all expensive work is async; recall-time check is ~1ms)

### Bias Considerations

The fact-checker uses a vocabulary bootstrapped from ConceptNet (optional, Tier 1) and organically grown from your conversations (Tier 2). ConceptNet is used ONLY as a dictionary for term reconciliation — it never evaluates truth claims.

The verification pipeline uses the same local Ollama model you configure. Its biases are your model's biases. The system does not impose external truth claims from any political, cultural, or ideological source.

Claims classified as "opinion" (detected via keyword heuristics: "better", "worse", "should", "underrated", etc.) are excluded from the verification pipeline entirely. The system will not tell you your preferences are wrong.

---

## Network Security

### Default: Localhost Only

By default, Hippocampus connects to Redis on localhost (or your configured internal address). No data leaves your machine/network.

### Network Mode (Optional)

If you enable Network Mode in Settings:
- TLS is enabled for the Redis connection
- A password is required
- The MCP server binds to a configurable address

This is for users who want to share a Redis instance across machines (e.g., laptop + desktop on the same Tailscale network).

### What Never Happens

- Hippocampus never phones home
- No telemetry, analytics, or usage reporting
- No cloud services required (everything runs locally)
- The Ollama calls go to YOUR Ollama instance (localhost by default)
- The only network connections are: Redis (your server) and Ollama (your server)

---

## Reporting Security Issues

If you find a security vulnerability in Hippocampus, please report it via GitHub Security Advisories (private disclosure) or email: security@ruthlesslypractical.com

Do NOT open public issues for security vulnerabilities.
