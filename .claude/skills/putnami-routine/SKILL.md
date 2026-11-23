---
name: putnami-routine
description: Create the person's Putnami routine, the one Claude routine Putnami fires for every routine task of theirs in a workspace, and print the two steps left to them. Use when the person asks to set up, create or update their Putnami routine.
allowed-tools: Bash, Read, Grep, Glob, ToolSearch
---

# Putnami routine

A person keeps one Putnami routine per workspace. Putnami fires it for each
routine task of theirs: the rebase of their pull request the merge queue found
in conflict with its base, and the build of an issue they start from
putnami.app or with `putnami agency build`. Its saved prompt,
`references/saved-prompt.md`, sends the routine to `references/dispatch.md`,
which hands each request to `scripts/dispatch.sh`. The script names the skill
to run only when the request's skill is on the allowlist this skill ships:
`rebase` runs the rebase skill, and `build` runs the routine-build skill. Both live under `.agents/skills/putnami-routine/` in the
workspace, whichever host reads this file. A new version of this skill reaches every
routine through the repository it checks out, with no change to the routine.

You run in the person's own session, with their own claude.ai account. Never
ask for, read or handle their claude.ai credentials or the routine's trigger
token, and never fire the routine.

## Before you create it

- Work from the workspace root: the directory that holds
  `putnami.workspace.json`. Its `agentArtifacts` must list
  `extension:@putnami/cloud`, or the routine's checkout carries neither
  this skill nor the skills it dispatches: say so and stop.
- The repository is the one `git remote get-url origin` names.

## Create the routine

Use the host's routines tool, the one that creates a claude.ai routine (in
Claude Code, `RemoteTrigger` with the `create` action). When the person
already has a Putnami routine for this repository, update it instead, so its
API trigger stays. Set exactly:

| Field | Value |
|---|---|
| Name | `Putnami routine` followed by the repository, `owner/name` |
| Repository | the workspace repository, from `origin` |
| Prompt | the whole content of `.agents/skills/putnami-routine/references/saved-prompt.md`, unchanged |
| Allowed tools | `Bash`, `Read`, `Write`, `Edit`, `Glob`, `Grep`, `Agent` |
| Connectors | the Putnami connector |
| Schedule | none: Putnami fires it through its API trigger |

The allowed tools and the connector are the widest the allowlisted skills
need: the rebase skill runs git, jq and curl through `Bash`, and reads files;
the routine-build skill edits the repository, runs the build skill's reviewer
as a separate agent, and signs the CLI in through the Putnami connector to
publish its pull request. Only routine-build uses the connector: the rebase
skill never does. Add no tool or connector for a skill that is not on the
allowlist.

When the host has no routines tool, ask the person to create the routine on
claude.ai, with **New task**, then **Set up manually**: the name and the
prompt above, **API** as its trigger, a cloud environment whose setup script
is `.agents/skills/putnami-routine/references/environment-setup.sh`, which installs the Putnami CLI that
routine-build signs in to publish, and whose network reaches the hosts that
https://putnami.dev/docs/platform/agent-environments#allow-the-putnami-domains
lists, then, under **Advanced settings**, the repository and the Putnami
connector alone. That form has no allowed
tools: the routine's cloud environment sets them. The API trigger then exists
at creation: the person copies its URL and its token there, as the first step
below says.

## The two steps left to the person

The API trigger and its token exist only in claude.ai's interface. Print
these steps, with the routine's name, then stop:

1. On claude.ai, open the routine and copy its **API trigger**'s Fire URL and
   token. A routine created with the routines tool has none yet: add it with
   **Add trigger**, **API**, then **Generate token**. claude.ai shows the
   token once.
2. Paste them in putnami.app: click **Start** on an issue of the backlog,
   and the window that opens takes them on its last step. Or, in your own
   terminal, at the workspace root, register them with Putnami:

   ```bash
   printf %s "$ROUTINE_TOKEN" | putnami agency trigger register \
     --url https://api.anthropic.com/v1/claude_code/routines/<routine>/fire --token-stdin
   ```

   Never paste the token into a conversation. `putnami agency trigger
   show` then shows the routine Putnami starts, never its token.

A trigger registered for one skill, with `--skill`, answers before the
Putnami routine for that skill.
