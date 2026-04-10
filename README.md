# session-search

Indexes local agent session history from pi, Claude Code, and Codex into a
flat text index for fast full-text search. Incremental indexing picks up both
new session files and existing session files that changed since the last index.
When an already-indexed session file changes, the current implementation
rewrites the full index file. That is intentionally blunt but correct for now;
the next optimization is a more incremental changed-file update path.

## Setup

```bash
go install github.com/alex-vit/session-search@latest
```

## Usage

```bash
session-search --index-only >/dev/null 2>&1 || true
session-search "oauth callback"
session-search --group "merge queue"
session-search --max 20 "sentry trace"
session-search --json "release checklist"
```

## Example Skill

One of the main use cases is wiring `session-search` into a skill so an agent
can recover prior context instead of guessing.

- [examples/SKILL.md](./examples/SKILL.md)

## What It Indexes

- `~/.pi/agent/sessions`
- `~/.claude/projects` excluding `/subagents/`
- `~/.codex/sessions`

The index lives at:

- `~/.config/session-search/index.txt`

The index currently includes:

- user and assistant text
- selected tool output
- command inputs
- thinking blocks when present

## Hook Wiring

`session-search` is most useful when indexing runs automatically from local
session lifecycle hooks or similar automation.

Recommended trigger points: session start, session end, clear / new chat.

Example hook action:

```bash
session-search --index-only >/dev/null 2>&1 || true
```

## Why Go

- Go binary startup: about `5ms`
- Python startup: `500ms+`
