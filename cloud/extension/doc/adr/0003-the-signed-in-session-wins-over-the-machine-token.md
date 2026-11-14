# ADR 0003 — The signed-in session wins over the workspace machine token

- **Status**: accepted
- **Date**: 2026-11-14
- **Scope**: `cloud/extension`: the credential a workspace command sends
  (`clicore.WorkspaceAuth`), the commands that report it (`whoami`, `status`),
  and the build cache provider's choice of a cache token

## Context

`PUTNAMI_CLOUD_TOKEN` carries a workspace machine token. A CI job or a runner
holds that one secret and no stored session. A hosted agent session holds
both: the host exports the machine token, and the person signs in with
`putnami cloud login` so the agent can read what only a person may read.

The workspace commands sent the machine token whenever the variable was set.
On such a session `cloud logs`, `cloud ci status`, `config`, `secrets`, `db`,
`env` and `source` answered 401 or "Insufficient permissions", although a
session that could answer them was stored.

## Decision

1. **A usable stored session wins.** A session is usable when
   `~/.putnami/auth.json` holds a fresh access token or a refresh token. With
   one, every workspace command authenticates with it, and
   `PUTNAMI_CLOUD_TOKEN` is ignored.
2. **The machine token serves the rest.** With no usable session, a command
   sends the machine token as before. No flag or variable chooses the machine
   token over a usable session: the machine token is going away from hosted
   sessions, and an opt-in would keep it there.
3. **The choice is visible.** `whoami` and `status` name the credential and
   why, `--verbose` prints one line naming it, and an authentication error
   names the credential the server refused.
4. **The build cache follows the same rule.** The cache provider reads with
   the machine token only when no usable session is stored.

## Rejected alternatives

- **Per-command precedence, the session for "person" commands only.** The
  split would be a list to maintain, and a command left off it would keep the
  surprising answer. One rule is easier to state and to check.
- **Fall back to the machine token when the session is refused.** A command
  would then answer with a different identity than the one the person expects,
  silently. The error names the credential instead.
- **A `--credential=machine` opt-in.** It would keep the machine token in use
  on hosted sessions, from which it is going away. A script that needs the
  machine identity runs where no session is stored, as CI does.

## Consequences

- A CI job or a runner with no stored session behaves exactly as before.
- A machine that holds both credentials uses the person's.
