---
name: session-search
description: Use session-search to recover relevant discussion, decisions, commands, and tool output from prior local sessions.
---

# Session Search

Use `session-search` when you need to recover context from earlier sessions
instead of guessing or re-deriving it.

## When To Use It

Trigger this skill when the user asks things like:

- "did we already talk about X"
- "search my past sessions"
- "what did we decide about X"
- "find where we discussed X"
- "what did I say about X last week"

Also use it when:

- a task is clearly a follow-up to prior agent work
- the user refers to a vague earlier thread without enough context
- you suspect useful shell output, web results, or excerpts were seen in a
  prior session

Do not use it for:

- general web research
- searching the current codebase
- searching docs or issue trackers directly when those sources are more
  authoritative than prior session memory

## How To Search

Start with a small number of specific terms. Prefer:

- ticket IDs
- proper names
- unusual phrases
- error strings
- command names
- internal tool names

Then broaden if needed.

Examples:

```bash
<search-command> "oauth callback"
<search-command> --group "deployment rollback"
<search-command> --max 20 "error trace"
<search-command> --json "handoff note"
```

## Search Strategy

1. Start with the most distinctive concrete term.
2. If that fails, try a nearby synonym or related noun phrase.
3. If results are noisy, add a second term rather than using a long natural
   language query.
4. Use grouped output when the important thing is finding the right session first.
5. Use JSON output only when another tool or script needs structured output.

## Hook Wiring

A local session index works best when it stays warm automatically.

Recommended lifecycle triggers:

- session start / resume
- session end / shutdown

Example hook action:

```bash
<search-command> --index-only >/dev/null 2>&1 || true
```

Replace `<search-command>` with the actual command name used in your setup.
