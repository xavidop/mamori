---
layout: ../../layouts/DocsLayout.astro
title: Agent skill
---

# Agent skill

mamori ships an [Agent Skill](https://www.skills.sh/) that teaches an AI coding agent (Claude Code, Cursor, Copilot, Windsurf, Gemini, and others) how to use mamori: defining config structs with `source:` tags, picking and wiring providers, keeping secrets in `secret.String`, watching for live changes, and driving the `mamori` CLI.

The skill lives in this repository under [`skills/mamori/`](https://github.com/xavidop/mamori/tree/main/skills/mamori).

## Install

The one-line install works for any agent skills.sh supports:

```bash
npx skills add xavidop/mamori
```

This fetches the skill and drops it into your agent's skills directory. Your agent then loads it automatically when a task involves loading config or secrets, wiring a provider, or the mamori CLI.

## Install manually

If you would rather not use the CLI, copy the skill folder into your agent's skills directory yourself. For Claude Code that is `~/.claude/skills/`:

```bash
git clone https://github.com/xavidop/mamori
cp -r mamori/skills/mamori ~/.claude/skills/mamori
```

Other agents use their own location (for example a project-level `.cursor/` or `.github/` skills folder); see your agent's documentation.

## What it covers

- The model: `source:` tags, providers as blank imports, `Load` versus `Watch`, and `secret.String` redaction.
- Defining and loading a config struct, with validation.
- Watching for live changes and reacting per field.
- Choosing a provider, with a full scheme cheat-sheet in the skill's `references/providers.md`.
- The `mamori` CLI (`explain`, `schema`, `policy`, `vet`, `doctor`, `status`) and its exit codes.

## llms.txt

If your agent does not use skills, point it at the documentation directly. This site publishes the [llms.txt convention](https://llmstxt.org/):

- [`/llms.txt`](https://mamorigo.dev/llms.txt) - a short index of the docs with links, for an agent to navigate.
- [`/llms-full.txt`](https://mamorigo.dev/llms-full.txt) - the entire documentation as one Markdown file, for an agent to load in a single fetch.

A prompt that works in most coding agents:

```text
Add mamori to my Go project. Docs: https://mamorigo.dev/llms.txt
```

## See also

- [Quick start](/docs/quickstart/) - the same ground for a human reader.
- [Providers overview](/docs/providers/) - every scheme and how to authenticate it.
- [CLI](/docs/cli/) - the command reference the skill points agents at.
