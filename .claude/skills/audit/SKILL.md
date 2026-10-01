---
name: audit
description: Scan projects for quality issues — create, update, and close GitHub issues; update scorecards. --prune removes dead weight per domain in one umbrella issue and one batch PR
model: claude-opus-5-5[1m]
allowed-tools: Agent, Bash, Read, Grep, Glob, TodoWrite, ToolSearch, mcp__putnami__putnami_search, mcp__putnami__putnami_symbol, mcp__putnami__putnami_context, mcp__putnami__putnami_impact, mcp__putnami__putnami_related, mcp__putnami__putnami_summary, mcp__putnami__list_projects, mcp__putnami__describe_project, mcp__putnami__deps, mcp__putnami__find_owner, mcp__putnami__why_impacted, mcp__putnami__topo_sort, mcp__putnami__impacted, mcp__putnami__get_diagnostics
argument-hint: [--all | --domain <domain> | --project <project> | --group <group> | --impacted] [--reconcile | --scorecard-only | --prune [--typology <t>] [--apply]] [--skip-scorecard] [--no-cache] [--dry-run]
---

# Claude adapter

Read `.agents/skills/audit/SKILL.md` completely and execute that canonical
workflow with `$ARGUMENTS`. Claude-only metadata stays in this adapter; do not
copy or reinterpret the shared workflow here.
