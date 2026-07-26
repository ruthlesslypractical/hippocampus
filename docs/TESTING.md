# Hippocampus — UX Test Plan

Manual testing checklist for the Hippocampus desktop app and CLI tools.

## App Launch & Connection

### Local Redis Mode
- [ ] Fresh install: app starts, generates redis.conf with random password
- [ ] Redis auto-starts on port 16379 (non-standard to avoid collision)
- [ ] Status shows green dot, key count, tag count, entries today
- [ ] Quitting app stops the bundled Redis process

### Remote Redis Mode
- [ ] Switch to "Remote" in Settings → remote fields appear
- [ ] Enter host/port/password → Save → auto-connects on next launch
- [ ] TLS checkbox works (connects over TLS)
- [ ] "Skip certificate verification" works for self-signed certs
- [ ] Invalid host shows error, doesn't crash
- [ ] Blank host in remote mode shows helpful error message

### Local Ollama Mode
- [ ] Ollama auto-starts if installed (system or bundled)
- [ ] Green dot when connected, shows model name
- [ ] If Ollama not found, shows clear error (not crash)

### Remote Ollama Mode
- [ ] Switch to "Remote" in Settings → URL field appears
- [ ] Enter remote URL → Save → connects on next launch

## Settings

- [ ] Cmd+, opens Settings overlay
- [ ] Escape closes Settings overlay
- [ ] "← Back" button (top right) closes overlay
- [ ] Save Settings shows "✓ Saved" briefly, then returns to main screen
- [ ] Redis mode radio buttons toggle remote fields visibility
- [ ] Ollama mode radio buttons toggle remote fields visibility
- [ ] Author field persists across restarts
- [ ] Settings changes disconnect existing Redis (forces fresh connect)

### Network Sharing
- [ ] "Share on local network" checkbox appears in Redis section (local mode only)
- [ ] Enabling it generates TLS cert (first time only)
- [ ] Shows verify phrase (5-word verbal hash), local IPs, and port
- [ ] Disabling it hides the network info
- [ ] Other machines can connect using the displayed IP/port with TLS

## Integration

- [ ] Integration card shows buttons for each detected client
- [ ] Installed clients show "⚡ Install → [Name]"
- [ ] Uninstalled clients show greyed out "[Name] (not installed)"
- [ ] Clicking Install writes correct config to client's config file
- [ ] After install, button changes to "✓ [Name]" (green, disabled)
- [ ] "Show Config" button toggles raw JSON snippet
- [ ] Generated config uses correct binary paths from .app bundle
- [ ] Config merges with existing client config (doesn't clobber other MCP servers)

### Supported Clients
- [ ] Claude Desktop (`~/Library/Application Support/Claude/claude_desktop_config.json`)
- [ ] Claude Code (`~/.claude/settings.json`)
- [ ] Cursor (`~/.cursor/mcp.json`)
- [ ] Gemini CLI (`~/.gemini/settings.json`)
- [ ] Kiro CLI (`~/.kiro/settings/mcp.json`)
- [ ] Windsurf (`~/.windsurf/mcp.json`)

## Data Browser

- [ ] "📋 Data" button opens Data overlay
- [ ] Stats header shows entry/tag/link counts
- [ ] Entry list loads (infinite scroll, 50 at a time)
- [ ] Scrolling to bottom loads more entries
- [ ] Search input filters entries (200ms debounce)
- [ ] Enter key forces immediate search
- [ ] Tag dropdown filters by tag
- [ ] "← Back" returns to main screen
- [ ] Escape closes Data overlay

### Entry Operations
- [ ] Each entry shows truncated content, timestamp, tag chiclets
- [ ] 🗑️ button deletes entry (shows native confirm dialog)
- [ ] Deleted entry disappears from list
- [ ] Tag chiclet × removes tag from entry (poof animation)
- [ ] After tag removal, entry re-renders with updated tags

### Tag Picker (+)
- [ ] Clicking + shows tag picker bubble
- [ ] Picker shows all existing tags as clickable chiclets
- [ ] Tags already on this entry are excluded from picker
- [ ] Filter input narrows tag list as you type
- [ ] Clicking a tag chiclet adds it to the entry
- [ ] Entry re-renders with new tag
- [ ] Typing a new name + Enter creates and adds tag
- [ ] "Create & Add" button appears for non-existing tags
- [ ] Escape dismisses picker
- [ ] Clicking + again dismisses picker (toggle)

## Tag Editor

- [ ] "🏷️ Tag Editor" button opens Tag Editor overlay
- [ ] Shows all tags as chiclets with entry count
- [ ] Tags sorted by count (most entries first)
- [ ] "← Back" returns to Data screen
- [ ] Escape closes Tag Editor

### Tag Operations
- [ ] × on a tag: first click arms (turns red, shows "✓ delete?")
- [ ] × second click within 3s: deletes tag + poof animation
- [ ] After 3s without second click: auto-disarms (back to normal)
- [ ] Deleted tag removed from all entries in Redis
- [ ] Deleted tag removed from tags:all
- [ ] "+ New Tag" chiclet opens inline input
- [ ] Type name + Enter/click Add creates tag
- [ ] Duplicate tag shows red flash error
- [ ] Empty name shows error
- [ ] Escape closes input
- [ ] New tag appears with pop-in animation

## Backup & Restore

### Backup
- [ ] "💾 Backup" button shows macOS native save dialog
- [ ] Default filename includes date: `hippocampus-backup-YYYY-MM-DD.json`
- [ ] Exports all entries + all links as JSON
- [ ] File is valid JSON (parseable with `jq`)
- [ ] Button shows "✓ Exported" on success

### Restore
- [ ] "📥 Restore from Backup" button in Settings → Data section
- [ ] Shows macOS native open dialog (filtered to .json)
- [ ] Imports entries, rebuilds tag sets, timeline, and links
- [ ] Shows result message with count of imported entries/links
- [ ] Imported entries visible in Data browser immediately

## Clear All Memory
- [ ] "🔴 Clear All Memory" button at bottom of Data screen
- [ ] First confirmation: native-style two-click (arm → fire)
- [ ] Successfully clears all data (FLUSHDB)
- [ ] Stats update to zero after clear

## MCP Server (hippocampus-mcp)

### Core Tools
- [ ] `memory_store` — stores entry with tags
- [ ] `memory_get` — retrieves by ID
- [ ] `memory_delete` — removes entry + tags + timeline
- [ ] `memory_search` — full-text search returns results
- [ ] `memory_by_tags` — intersection (match_all=true) works
- [ ] `memory_by_tags` — union (match_all=false) works
- [ ] `memory_list_tags` — returns all tags with counts
- [ ] `memory_add_tags` — adds tags to existing entry
- [ ] `memory_remove_tags` — removes tags from entry
- [ ] `memory_rename_tag` — renames across all entries
- [ ] `memory_time_range` — returns entries in time window
- [ ] `memory_link` — creates bidirectional link with score
- [ ] `memory_unlink` — removes link
- [ ] `memory_links` — returns links for entry sorted by |score|

### Web Ingestion
- [ ] `memory_ingest_url` — fetches, extracts, chunks, stores
- [ ] Stub entry created with content:stub tag
- [ ] Content entries created with content:full tag
- [ ] Stub contains title, URL, excerpt, chunk pointers, untrusted warning
- [ ] Content entries linked to stub (extends relation)
- [ ] Sequential chunks linked to each other (preceded_by)
- [ ] High-risk content (score ≥ 0.8) rejected with error
- [ ] Medium-risk content (score ≥ 0.5) sanitized before storing
- [ ] Clean content stored without modification
- [ ] User-supplied tags applied to both stub and content entries
- [ ] domain: and url: tags auto-applied

## Hook (hippocampus-hook)

### Recall (userPromptSubmit)
- [ ] First prompt in session injects full orientation
- [ ] Subsequent prompts inject lean kernel only
- [ ] Relevant memories injected based on keywords/tags
- [ ] `content:full` entries never auto-injected (Layer 5)
- [ ] `content:stub` entries surface with untrusted framing
- [ ] Associative links followed (up to max_link_hops)
- [ ] Coverage indicator appended (entries since last summary)
- [ ] Confidence decay applied to older entries

### Store (stop)
- [ ] Assistant responses auto-captured with tags
- [ ] Short responses (< min_response_len) not stored
- [ ] Correct tags applied: kind:assistant_response, auto:captured, session, date, cwd
- [ ] User prompts stored with kind:user_prompt tag
- [ ] Short prompts (< min_prompt_len) not stored

## Summarizer (hippocampus-summarize)

- [ ] `--daily` summarizes today's entries per track
- [ ] `--3h` summarizes last 3 hours per track
- [ ] `--weekly` rolls up daily summaries
- [ ] `--cross-track` detects themes across tracks
- [ ] `--all` regenerates all track summaries
- [ ] `--track X` limits to specific track
- [ ] `--dry-run` prints without writing
- [ ] `--consolidate` runs continuous pairwise link discovery
- [ ] Summarizer excludes `content:full` entries (Layer 4 security)
- [ ] Orphan entries get classified into tracks
- [ ] Summaries stored with summary:track:X and summary:daily tags

## CLI (hippocampus-ingest)

- [ ] `hippocampus-ingest --tags "a,b" -v <url>` works
- [ ] Flags must come before URL (Go flag behavior)
- [ ] `-v` shows verbose output with content IDs
- [ ] `--dry-run` extracts but doesn't store
- [ ] `--max-chunk N` controls chunk size
- [ ] Reads config from HIPPOCAMPUS_CONFIG env var or ./config.json
- [ ] Exit code 1 on failure with error message

## Security

- [ ] Prompt injection in ingested pages detected (high-risk rejected)
- [ ] Injected medium-risk content sanitized (patterns redacted)
- [ ] Web content never auto-injected into model context
- [ ] Web content never included in summarization input
- [ ] Stub entries clearly marked as untrusted
- [ ] TLS forced when network sharing enabled
- [ ] Redis password auto-generated on first local start
- [ ] No credentials stored in git (check .gitignore)

## Daemon (hippocampus-daemon)

### Lifecycle
- [ ] Daemon starts via LaunchAgent when app starts it
- [ ] `launchctl list com.ruthlesslypractical.hippocampus.daemon` shows running
- [ ] Log output at `~/Library/Logs/Hippocampus/daemon-stdout.log`
- [ ] Binary self-update detection works (rebuild → daemon exits → launchd restarts)
- [ ] Daemon actually exits on binary change (not just logs "exiting")
- [ ] Clean shutdown on SIGTERM (drains in-flight GPU jobs)
- [ ] Subsystem toggle: disable classifier in config → daemon skips classify jobs on next pick

### Priority Dispatcher
- [ ] Live ingest entries (from queue) processed before backlog
- [ ] Verification jobs picked before backlog
- [ ] Backlog entries processed oldest-first
- [ ] Condenser runs at priority 4 (after verify, before consolidation)
- [ ] Consolidation only runs when nothing else is queued
- [ ] `gpu_concurrency` limits parallel Ollama calls (verify with activity monitor)

### Condenser
- [ ] New entries get `summary` field after daemon processes them
- [ ] Short entries (user <300, assistant <1500, other <500 chars) skipped
- [ ] Summary length capped at `max_output_chars` (default 250)
- [ ] Data dumps produce metric-rich summaries (not just "training continues")
- [ ] Config/code entries describe what they configure
- [ ] Backfill: entries without `summary` field gradually get one

### Consolidation & Links
- [ ] Co-recall auto-links created at threshold (default 3 co-recalls)
- [ ] Short entries (<200 chars) never get co-recall links
- [ ] Re-evaluation: existing links re-scored periodically
- [ ] Re-evaluation: short entries dissolved immediately ("short content" log)
- [ ] Discovery: random pairing produces genuine cross-track links
- [ ] Discovery: entries <200 chars excluded from discovery
- [ ] Discovery: scores vary (not all 0.5) — graded scoring prompt working
- [ ] Same-session dissolution protection: demotes before dissolving (check logs)
- [ ] Link dissolution: links below min_score (0.4) get removed
- [ ] Temporal neighbors: same-session + same-track entries get weak links
- [ ] Ollama backoff: after 3 consecutive failures, exponential backoff kicks in

### Classification
- [ ] New entries classified into tracks
- [ ] `track_auto:X` tag applied
- [ ] `classified` and `classified:auto` tags applied
- [ ] Windowed context: classifier sees surrounding entries for context
- [ ] Short messages inherit track from neighbors

## Admin CLI (hippocampus-admin)

### Orientation
- [ ] `orientation list` shows all meta:orientation:* entries with table
- [ ] `orientation show <id>` prints full content
- [ ] `orientation add meta:orientation:track:Test` opens $EDITOR with template
- [ ] `orientation add meta:orientation:track:Test --file x.md` stores file content
- [ ] `orientation add` rejects invalid ID format
- [ ] `orientation add` rejects if entry already exists
- [ ] `orientation edit <id>` opens current content in $EDITOR, saves on change
- [ ] `orientation edit <id>` detects no-change and aborts
- [ ] `orientation delete <id>` confirms before deleting
- [ ] Auto-tags: meta, orientation, track:<Name> applied on add

### Entry/Tag Operations
- [ ] `entry show <id>` prints content, tags, timestamp, links
- [ ] `entry list --track X` filters correctly
- [ ] `entry edit <id>` opens JSON in editor, applies changes
- [ ] `entry delete <id>` removes from hash + timeline + tag sets + links
- [ ] `tag rename old new` updates all entries
- [ ] `tag delete <tag>` removes from all entries + set key
- [ ] `tag promote --track X` bulk promotes track_auto → track

## Track Orientation & Shift Detection

- [ ] On track shift, `[TRACK SHIFT: A → B]` appears in context
- [ ] If orientation exists for new track: full content injected inline
- [ ] If no orientation: helpful hint about `hippocampus-admin orientation add`
- [ ] Track detection uses dominant track from recalled entries
- [ ] Last track stored per-session in Redis (24h TTL)
- [ ] First prompt of session: no shift detected (no previous track)

## Tiered Recall

- [ ] Tier 1 (position 0): full content injected verbatim
- [ ] Tier 2 (positions 1-4): condensed summary used if available
- [ ] Tier 2: falls back to truncation (300 chars) if no summary
- [ ] Tier 2: includes `[full entry: <id>]` pointer
- [ ] Tier 3 (positions 5+): 80-char snippet + entry ID ref
- [ ] Orientation entries bypass tiering (always full)
- [ ] Linked results use separate budget (don't compete with search)
- [ ] Relevance floor: entries with negligible weight excluded

## Working Set Tracker

- [ ] Working set injected into every prompt after first
- [ ] Content is ≤5 bullets, ≤500 chars
- [ ] Updates after each assistant response (stop hook)
- [ ] Previous session's working set inherited within 24h
- [ ] Ollama timeout configured via working_set.timeout_s
- [ ] Graceful degradation: if Ollama fails, stale working set preserved

## OFC (Neuromodulator)

- [ ] `[NEUROMODULATOR STATE]` block injected when enabled
- [ ] DA moves on explicit positive/negative feedback
- [ ] DA decays toward 0 each prompt
- [ ] 5HT drifts toward baseline (0.5)
- [ ] DA directive text changes based on level (positive/negative/neutral)
- [ ] 5HT directive text changes based on level (good/normal/low)
- [ ] OFC state persists across prompts within session
- [ ] Regex fallback works when Ollama model unavailable

## Packaging

### macOS (.app bundle)
- [ ] `make bundle` produces `dist/Hippocampus.app`
- [ ] All 8 binaries present in Contents/Resources/
- [ ] Redis + redisearch.so + dylibs present
- [ ] Ollama present
- [ ] Code signing passes (ad-hoc or real identity)
- [ ] App launches on clean macOS install (no prior deps)

### RPM
- [ ] `./packaging/build-srpm.sh` produces .src.rpm
- [ ] `rpmbuild` or mock builds binary RPM
- [ ] Systemd units installed and functional

### Debian
- [ ] `./packaging/build-deb.sh 2.0.0` produces .deb
- [ ] `dpkg -i` installs cleanly on Ubuntu 25.10
- [ ] Systemd units (daemon + summarize timer) enabled and started
- [ ] `apt-get install -f` resolves redis-server dependency
- [ ] Config at /etc/hippocampus/config.json preserved on upgrade
