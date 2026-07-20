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
