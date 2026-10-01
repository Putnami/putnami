# ADR 0012 — Install trust: a curl-pipe installer that refuses what it cannot verify, and a smoke that runs the whole golden path

- **Status**: accepted
- **Scope**: `@putnami/cli` (`tooling/cli`) — `scripts/install.sh`, `scripts/install-commands.txt` and `scripts/smoke-check-release.sh`; `putnami.dev` (`sites/putnami.dev`), which serves the script at `/install.sh`

## Context

`curl -fsSL https://putnami.dev/install.sh | bash` is the first command in the
README, on the website and in every getting-started page. The user runs it
before they have anything to audit it with, and it runs with their full write
access. Its output is the only evidence they get, so every word in it must be a
fact: "verified" must mean "compared", and a refusal must come before anything
is written.

The in-binary `putnami upgrade` already reads the registry's advertised digest
and refuses fail-closed when it is absent. The public install path must be no
weaker. A release gate that never executes the installer proves nothing about
it. [`../22-installing-the-cli.md`](../22-installing-the-cli.md) is the
specification. Windows installation is owned by
[ADR 0052](0052-windows-consumers-junctions-job-objects-lf-and-long-paths.md)
§7.

## Decision

### 1. The installer is fail-closed on integrity, and says "verified" only after a comparison

The installer reads the digest the registry advertised for the streamed bytes
(`X-Integrity`, else the `sha-256` member of an RFC 9530 `Digest` header),
normalizes it as `extension.NormalizeIntegrity` does, computes a local SHA-256,
and compares. `Integrity verified (sha256:…)` is printed only on the success
branch.

| Situation | Behavior |
|---|---|
| Advertised digest ≠ local digest | Exit non-zero. Nothing is installed. |
| No digest advertised | Exit non-zero, naming `PUTNAMI_UNSAFE_INSTALL=1`. |
| No `sha256sum` or `shasum` on the host | Exit non-zero, before downloading. |
| `--download-url` with no `--sha256` | Exit non-zero, naming `--sha256`. |

`PUTNAMI_UNSAFE_INSTALL=1` is the single override, for any case where nobody
vouched for the bytes. It installs with a warning on stderr. It is the same
variable `putnami upgrade` honors, so a user learns one escape hatch.

The installer also refuses a binary whose embedded `--version` disagrees with
the version the registry resolved, so a stale artifact cannot install under a
fresh tag. A binary that reports no version is not blocked. The registry must be
reached over https, or http on loopback only.

Best-effort verification is rejected: it succeeds on a tampered download
whenever the tampering also strips the header, which is the only case that
matters, and it makes "verified" unreadable in a log.

### 2. The installer never escalates privileges

The script contains no `sudo`, `doas`, `pkexec` or `run0`; a test scans for
them. It writes to `~/.putnami/bin`, links only into a directory that is both on
`PATH` and writable by the invoking user, and appends to that user's shell
startup file. When no writable target exists, it names the directory, the reason
and `--install-dir`, and exits before downloading.

A piped script that prompts for a password asks for consent the user cannot
inform: the bytes being authorized were already consumed from stdin. The cost is
accepted: a non-administrator cannot install into `/usr/local/bin`, and the
installer says so.

### 3. The supported matrix is enforced, not warned about

`install.sh` installs `darwin|linux` × `amd64|arm64`, matching the
[First Public-Release Contract](../../../../README.md#first-public-release-contract).
Any other `uname` combination exits non-zero, naming the detected values and the
supported set. The check runs before the prerequisite check and before any
network call. A warning followed by an install surfaces the real failure at the
first `putnami` invocation, where the warning is gone.

The installer sends Go's `GOARCH` names (`amd64`, `arm64`), as `putnami upgrade`
does. The registry maps `amd64` onto its stored `x64`; the specification
documents the alias.

A rollback installs an exact version with `--version <tag>`
([ADR 0011](0011-release-governance-and-neutral-ci.md) §1). The installer has
no compatibility flag.

### 4. Same-shell discovery is reported honestly

The installer makes `putnami` reachable in this order: the install directory is
already on `PATH`; a user-writable directory on `PATH` gets an atomically swapped
symlink; otherwise it prints the exact `export PATH=…` line and appends it once
to the right startup file. It then reports what `putnami` resolves to, compared
by inode, and names an older copy earlier on `PATH` as a shadow. It never claims
the command is available.

- When `~/.local/bin` or `~/bin` is on `PATH` but absent, the installer creates
  it before linking. It never creates any other absent `PATH` directory.
- Startup files are only appended to, keyed on the line, so a re-install does
  not duplicate it. The zsh `fpath` line is appended with its own `compinit`,
  and only when zsh reports no writable completion directory.

### 5. The release smoke fetches the public installer and runs the golden path

The smoke runs, in order:

1. the channel resolves and advertises a digest that matches the streamed bytes;
2. `curl -fsSL https://putnami.dev/install.sh | bash` installs from that channel,
   with a recording `sudo` on `PATH` that fails the leg if called, and reports
   `Integrity verified`;
3. `putnami` is discovered in the smoke's own workdir, hashes to the host's
   asset, and its stamp matches the resolved version;
4. `putnami init --project webapp --extension ts` writes the required workspace,
   project, lock, dependency and MCP state;
5. `putnami serve webapp` emits typed readiness, answers a real HTTP request,
   and stops without the hard fallback.

- The installer under test is the served public URL. `SMOKE_INSTALL_URL` may
  replace only that URL; `PUTNAMI_VERSION` selects the channel.
- Every leg failure prints a bounded `::error::` diagnostic. `SMOKE_DIAGNOSTICS_DIR`
  opts into retained logs; otherwise the temp tree is removed on success,
  failure, signal and timeout.
- Tests serve `install.sh` and the registry locally (`SMOKE_INSTALL_URL`,
  `PUTNAMI_REGISTRY_URL`) with a helper that accepts only the exact init and
  serve argv, so every failure branch is executable before a release depends
  on it.

`putnami.ci.json` has neither schedules nor an operating-system matrix, so an
external release execution plane runs the candidate gate on the four macOS and
Linux targets (the Windows job is ADR 0052 §7) and the nightly `latest` monitor
on the same four, and a failure escalates to the release
owner ([ADR 0011](0011-release-governance-and-neutral-ci.md)). Without a working
plane, the same runs are required manually. Everything assertable before
publication stays in the CLI's local test suite.

### 6. The run form resolves its extension from an unsigned command map

`curl -fsSL "https://putnami.dev/install.sh?run=<command>" | bash` (or
`--run <command>`) installs the CLI, pins the extension that provides
`<command>` for the user, and runs `putnami <command>` in the caller's
directory. The command map `https://putnami.dev/install-commands.txt` names that
extension; it is published from `scripts/install-commands.txt`.

The map is not signed. It is served from the script's origin under the same
https-or-loopback rule, so anyone who can change it can already change the
script. Its authority is bounded:

- An entry names an extension, `@scope/name[@constraint]`, and nothing else: no
  URL, path or shell text. The installer validates the whole map, fail-closed,
  before it uses any line, and never evaluates a value from it.
- The CLI installs that extension through the registry and verifies its SHA-256
  like any extension install.
- A command name matches `^[a-z][a-z0-9-]{0,63}$` in the site, the script and the
  map. The site writes only a matching `?run=` value into the script.
- A refused command or map exits before the CLI is downloaded.

The framework ships the map empty; an extension owner adds an entry by pull
request. The smoke's opt-in `run` leg (`SMOKE_RUN_COMMAND`) runs the run form in
a fresh directory and requires exit 0 and an untouched directory.

## Consequences

- The public install path and `putnami upgrade` read the same headers, normalize
  the same way, refuse fail-closed and share one override.
- A registry that stops advertising `X-Integrity` fails the smoke. The registry
  header is a release-blocking dependency of the installer.
- Installing into a directory the user does not own needs `--install-dir` run by
  someone who can write there.
- A host without `sha256sum` or `shasum` installs only with the override,
  documented in `--help`.
- The smoke installs a TypeScript extension and starter against isolated home,
  config, cache and store state, so it is slow.
- The installer proves the bytes are the ones the registry advertised over an
  authenticated channel, not that they came from Putnami. Artifact signing is
  out of scope, and the specification says so.
