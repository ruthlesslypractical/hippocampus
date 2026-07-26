# Orientation Template

This template is used by `hippocampus-admin orientation add <id>` to pre-fill the editor.
It defines the structure of a track orientation entry — the invariant "what is this and how
do I interact with it" context that gets injected when an agent shifts to this track.

## Usage

```bash
# Create a new orientation (opens $EDITOR with this template)
hippocampus-admin orientation add meta:orientation:track:MyProject

# Create from a file
hippocampus-admin orientation add meta:orientation:track:MyProject --file ./my-orientation.md

# List all orientations
hippocampus-admin orientation list

# View one
hippocampus-admin orientation show meta:orientation:track:MyProject

# Edit in place
hippocampus-admin orientation edit meta:orientation:track:MyProject

# Remove
hippocampus-admin orientation delete meta:orientation:track:MyProject
```

## Template

```markdown
## Track: <Name>

**Nature:** <software | research | analysis | concept | infrastructure | hybrid>

**Artifacts:**
- <where the primary stuff lives — repos, directories, documents, URLs>
- <secondary locations if applicable>

**Interfaces:**
- <how to build/run/deploy/interact>
- <key commands, endpoints, or tools>

**Vocabulary:**
- <term>: <what it means in this track's context>

**Notes:**
- <anything invariant that trips you up without context>
```

## Guidelines

- Keep it **static**: orientation describes what things ARE, not what's currently happening.
  Current state goes in the working set (`meta:working-set:*`).
- Keep it **concise**: aim for 800-1500 chars. This gets injected into context on track shifts.
- **Vocabulary** is the most underrated section. Terms that mean something specific in your
  project (and could be confused with general usage) go here.
- **Artifacts** should answer "where do I find things?" — repos, hosts, config paths, URLs.
- **Interfaces** should answer "how do I do things?" — build commands, deploy steps, key tools.

## Auto-tags

When stored via `orientation add`, the entry automatically receives:
- `meta` — marks it as system/infrastructure data
- `orientation` — type marker for discovery
- `track:<Name>` — extracted from the ID (if format is `meta:orientation:track:<Name>`)

## ID Format

IDs must match: `meta:orientation:track:<Name>` or `meta:orientation:<other>`

Examples:
- `meta:orientation:track:Hippocampus`
- `meta:orientation:track:OtherTrack`
- `meta:orientation:general` (for non-track orientations)
