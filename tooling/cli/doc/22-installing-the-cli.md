# Installing the CLI

This document is the authority for three things: what
`curl -fsSL https://putnami.dev/install.sh | bash` does, including its
[run form](#run-one-command-without-a-workspace), what
`irm https://putnami.dev/install.ps1 | iex` does on Windows, and what the release
smoke proves about them. [ADR 0012](adr/0012-install-trust-and-release-smoke.md)
and [ADR 0052](adr/0052-windows-consumers-junctions-job-objects-lf-and-long-paths.md)
record why.

The installers are [`tooling/cli/scripts/install.sh`](../scripts/install.sh) and
[`tooling/cli/scripts/install.ps1`](../scripts/install.ps1). The site publishes
those exact files at `/install.sh` and `/install.ps1` through `generate.assets`
entries in `sites/putnami.dev/putnami.json`, so the bytes you pipe into a shell
are the bytes in this repository. Every claim below is pinned by a test; the
[Evidence](#evidence) table says which one.

## Install it

The gated first use is exactly:

```bash
curl -fsSL https://putnami.dev/install.sh | bash
export PATH="$HOME/.putnami/bin:$PATH"
putnami init --project webapp --extension ts
putnami serve webapp
```

Run it from an empty directory on macOS or Linux (`amd64` or `arm64`). On
Windows, run the PowerShell equivalent in
[Install on Windows](#install-on-windows). It needs
Bash, `curl`, `tar`, a SHA-256 tool (`sha256sum` or `shasum`), Bun v1.4.0 or later
for the TypeScript init/serve path, network access to the public site/registry,
and network access to the public package registry (`https://npm.putnami.dev`)
used by `bun install`. No directory has to be on `PATH` beforehand.

The first command installs the newest stable CLI into `~/.putnami/bin`, links it
into a user-writable directory that `PATH` already names when there is one
(`~/.local/bin`, `~/bin`, or a writable `/usr/local/bin`), installs shell
completions, and registers Putnami with an
installed Claude Code or Codex host through that host's official `mcp add`
command. For an existing Putnami workspace, that is the whole technical setup:
open the project in either host and ask for the work. `putnami init` remains the
explicit step that creates a new workspace and its product intent.

A pipe-to-shell child cannot change its parent shell. The second line of the
block, `export PATH="$HOME/.putnami/bin:$PATH"`, is what makes `putnami`
reachable in a shell where `PATH` names none of those directories; it changes
nothing in a shell that already reaches the command. The block assumes the
default install directory: with `--install-dir <dir>`, export that directory
instead. Once you have a CLI, you never need the script again —
`putnami upgrade --global` replaces it.

## Claude Code and Codex registration

The host registration points at the stable installed path, such as
`~/.putnami/bin/putnami`, and passes only `mcp`. It stores neither a repository
path nor a Cloud credential. The stable launcher enters the active session's
directory before resolving `putnami.lock.json`, so a worktree's exact CLI pin is
still authoritative. In a *source* workspace — one whose lock records
`cli.source: "workspace"`, such as the `putnami` repository itself — an installed
CLI is not authoritative and is refused; run `./putnamiw mcp` from that worktree
instead (see [Version management](14-version-management.md#source-workspaces-building-the-cli-instead-of-pinning-one)).


- Claude Code receives `PUTNAMI_AGENT_WORKSPACE=${CLAUDE_PROJECT_DIR:-.}` in its
  user-scoped MCP definition, and also injects `CLAUDE_PROJECT_DIR` directly
  into every spawned stdio server. Putnami prefers the direct value, which is
  the stable root where that Claude session launched.
- Codex starts the user-scoped stdio server in the session working directory.
  The definition deliberately has no `cwd`, so `codex -C`, resume, and a new
  worktree do not inherit the directory where Putnami happened to be installed.

Pass `--no-agent-hosts`, or set `PUTNAMI_NO_AGENT_HOSTS=1`, to decline this
entirely. Nothing else about the install changes, and no host configuration is
read or written. Use it for image builds, CI cache warms, and any machine whose
agent hosts you configure by hand.

The installer first runs the host's `mcp get putnami`. An existing definition
that matches the launcher contract is reported as verified. The comparison is by
meaning, not by formatting: Codex's `--json` output is a host-owned document
that may gain a field, reorder keys, or reindent between releases, so it is
compared after whitespace is removed and only on the fields that change
behavior — the command, its one `mcp` argument, an enabled server, and the
absence of a stored `cwd`, injected environment, or tool filter. A release that
reformats its output therefore does not make Putnami mistake its own entry for
someone else's and stop maintaining it.

A customized definition remains human configuration: Putnami leaves it
byte-for-byte untouched, reports that it was preserved, and gives the inspection
command. This also preserves comments and unrelated tables in Codex's TOML.

A missing definition is added and then read back. Codex's JSON output must
confirm an exact command, one `mcp` argument, no environment or cwd, and no host
tool filters before the installer reports its configuration verified. When
Claude is installed outside a Putnami workspace, its exact definition can be
verified even though the server cannot connect there; the installer reports
that the connection will be checked when Claude opens a Putnami workspace and
does not report connection readiness. Managed policy can refuse the write
without invalidating the verified CLI installation, but the installer then
warns and never calls the host ready.

Host precedence and trust still apply. Claude local/project entries override
the user entry, and a checked-in `.mcp.json` server requires the host's one-time
project approval. `putnami init`, `putnami install` and `putnami upgrade` add
the `putnami` entry to that file when it is missing, and `putnami mcp install`
repairs a diverged one. Codex
loads `.codex/config.toml` only after the project is trusted, and a project
entry can replace the user launcher. Inspect a preserved
override with `claude mcp get putnami` or `codex mcp get putnami`. A host session
that was already open must reconnect that MCP server or start a new session;
installing a binary cannot mutate the tool inventory already held in memory.

The acceptance inventory is pinned to Claude Code `2.1.260` and Codex CLI
`0.153.4`. Older hosts receive the same official commands and are reported as
unverified if they cannot store or read back the contract. Likewise, a workspace
that intentionally pins a Putnami CLI from before this integration continues to
run that exact engine: the launcher fixes its cwd before re-exec, but does not
invent newer tools or guidance. Move that workspace pin explicitly when the new
capabilities are required.

Claude keeps `CLAUDE_PROJECT_DIR` fixed for the lifetime of a spawned server.
Starting Claude directly in a worktree gives Putnami that worktree root, and
ordinary subagents share it. Entering another worktree after the MCP server has
started does not retarget the existing process; reconnect Putnami in that root,
including after a resume, or start a session there. A worktree-isolated subagent
needs its own MCP connection in that root; if the host shares the parent's
server, the agent uses the documented local fallback. This is a Claude host
boundary rather than a workspace path Putnami can safely guess or persist.
The MCP initialize message names its resolved root so analysis, resumed work,
and subagents can compare it with their active cwd/worktree before using a tool.
A mismatch skips that server immediately and uses the local Putnami CLI; a
reconnect in the active root is optional.

## Run one command without a workspace

Some extension commands work in any directory, with no Putnami workspace. The
run form installs the CLI and runs one of them, in one line:

```bash
curl -fsSL "https://putnami.dev/install.sh?run=<command>" | bash
```

The same run, spelled as an installer argument:

```bash
curl -fsSL https://putnami.dev/install.sh | bash -s -- --run <command>
```

A script piped into `bash` receives no arguments, so the first form carries the
command inside the script. For `/install.sh?run=<command>` the site serves the
bytes of `/install.sh` with one line changed: `RUN_COMMAND_DEFAULT=""` becomes
`RUN_COMMAND_DEFAULT="<command>"`. `--run <command>` and `PUTNAMI_RUN=<command>`
set the same value. `--run` wins over `PUTNAMI_RUN`, and both win over the value
in the script.

The installer then works in this order:

1. **It checks the command name.** The name must match `^[a-z][a-z0-9-]{0,63}$`.
   Anything else exits non-zero before any network call.
2. **It resolves the extension.** It downloads the
   [command map](#the-command-map) and looks up the extension that provides the
   command. A command the map does not list, a map that fails validation, and a
   map it cannot download all exit non-zero before the CLI is downloaded and
   before the install directory is created.
3. **It installs the CLI** exactly as the plain form does: the same integrity
   check and refusals, `PATH` discovery, completions, and Claude Code and Codex
   registration. `--no-agent-hosts` still applies.
4. **It pins the extension for your user** with
   `putnami extensions install --user --latest <@scope/name[@constraint]>`. The
   CLI downloads the extension archive and verifies its SHA-256 fail-closed,
   like any extension install (see [The User Scope](07-extensions.md#the-user-scope)).
   `--latest` moves an existing pin, so each run of the one line gets the
   newest release the map entry allows.
   If the pin fails, the installer exits with the pin's status and does not run
   the command. The CLI stays installed.
5. **It runs `putnami <command>`** in the directory you started from, and exits
   with that command's exit status.

In this mode the streams are split:

- The installer's own messages go to stderr. Stdout carries only the command's
  output, so `curl … | bash > out.txt` captures the command's output and nothing
  else. The closing "Done" and "Next" lines are not printed.
- The command's stdin is your terminal (`/dev/tty`) when one can be opened, and
  `/dev/null` otherwise. It is never the pipe the script arrives on, so the
  command cannot read the rest of the script as input.

The installer writes nothing to the directory you start from. It writes where
the plain form writes, plus the user-scope pin. A command the map lists runs
without a workspace and must leave that directory untouched as well; the
release smoke checks this for the command it runs
([The run scenario](#the-run-scenario)).

### The command map

The installer reads `https://putnami.dev/install-commands.txt`. The site
publishes [`tooling/cli/scripts/install-commands.txt`](../scripts/install-commands.txt)
there through a `generate.assets` entry, next to `install.sh`.
`PUTNAMI_COMMAND_MAP_URL` points the installer at another map.

The map is a line format. Its first line names the format version:

```
putnami.install-commands.v1
# A comment.
<command> <@scope/name[@constraint]>
```

| Line | Rule |
|---|---|
| First line | Exactly `putnami.install-commands.v1`. Any other first line, including a later version, is refused. |
| Empty, only spaces and tabs, or `#` after optional spaces and tabs | Ignored. |
| Entry | Two fields separated by spaces or tabs, with nothing before or after them. |
| `<command>` | Matches `^[a-z][a-z0-9-]{0,63}$`, and appears at most once. |
| `<@scope/name[@constraint]>` | `@scope/name` in lowercase letters, digits, `.`, `_` and `-`. An optional `@<constraint>` is an optional `^` or `~`, then letters, digits, `.`, `+`, `_` and `-`. |

The installer validates the whole map before it uses any line. A malformed line
anywhere, or a command listed twice, refuses the run, so what a command resolves
to never depends on where a mistake sits. A map announced as larger than 1 MiB
is refused. The shell never evaluates a value from the map: the extension
reference reaches the CLI as one argument.

An entry without `@<constraint>` pins the extension's `latest` channel. The
registry decides who can read that channel: when it is private, the pin fails
for a reader without access, and the command does not run.

The map the framework ships lists these commands:

| Command | Extension | One line |
|---|---|---|
| `agent-readiness` | `@putnami/intelligence` | `curl -fsSL "https://putnami.dev/install.sh?run=agent-readiness" \| bash` |

An extension owner adds an entry by pull request against
`tooling/cli/scripts/install-commands.txt`, and adds the command to the table
above and to the list in `internal/installscript/run_command_test.go`. The
command must declare that it runs without a workspace, and must not write to
the directory it runs in.

### The command in the URL

The site answers `GET` and `HEAD` for `/install.sh` and `/install.ps1` as
follows:

| Request | Response |
|---|---|
| No `run` parameter, whatever else the query holds | The static script, byte-identical, with the same headers. |
| One `run` value matching `^[a-z][a-z0-9-]{0,63}$` | `200`: the script with that value in its placeholder line. Same `Content-Type` (`application/x-sh` for `install.sh`, `application/octet-stream` for `install.ps1`) and `Cache-Control` as the static script, and an `ETag` of its own. `HEAD` and `If-None-Match` work as for the static script. |
| Any other `run`: empty, repeated, too long, or with a character the rule does not allow | `400`, `text/plain`, `Cache-Control: no-store`. The body does not echo the value. |
| A script without exactly one placeholder line | `500`. The site never serves a script that would ignore the command. |

The placeholder line is `RUN_COMMAND_DEFAULT=""` in `install.sh` and
`$RunCommandDefault = ''` in `install.ps1`. Only a value that matches the rule
is ever written into the script, between double quotes in `install.sh` and
single quotes in `install.ps1`, where none of its characters is special to bash
or PowerShell. The script applies the same rule again. The route inventory
keeps one `exact /install.sh` route and one `exact /install.ps1` route: route
matching ignores the query, so each route covers every variant.

### Run one command on Windows

In a PowerShell window, type:

```powershell
irm "https://putnami.dev/install.ps1?run=<command>" | iex
```

For `/install.ps1?run=<command>` the site serves `/install.ps1` with one line
changed: `$RunCommandDefault = ''` becomes `$RunCommandDefault = '<command>'`.
`install.ps1` then works in the order above, with the same command map, the
same rules and the same refusals before anything is installed.

The same run, spelled as an installer argument, runs the script as a script
block:

```powershell
& ([scriptblock]::Create((irm https://putnami.dev/install.ps1))) --run <command>
```

From `cmd.exe`, download the script to `%TEMP%`, not to the current directory,
then run it as a file:

```
curl.exe -fsSLo "%TEMP%\install.ps1" https://putnami.dev/install.ps1 && powershell -NoProfile -ExecutionPolicy Bypass -File "%TEMP%\install.ps1" --run <command>
```

As for the plain install, do not pass the one-liner to `powershell -c`:
Microsoft Defender blocks that command line
([Install on Windows](#install-on-windows)).

What differs from `install.sh`:

- **Exit status.** Run as a file, with `powershell -File` or `& .\install.ps1`,
  the script exits with the command's exit status. Run as text, through
  `irm | iex` or the script block, the script does not call `exit`, because
  `exit` would close the PowerShell window. It leaves the command's exit status
  in `$LASTEXITCODE`, where PowerShell leaves the status of every program it
  runs. A failed pin ends the same way, with the pin's status. A refusal before
  the command runs throws an error, as in the plain form.
- **Streams.** The installer's own messages go to stderr, and the closing
  "Done" and "Next" lines are not printed. Stdout carries only the command's
  output.
- **`PUTNAMI_RUN`.** `$env:PUTNAMI_RUN = '<command>'` before
  `irm https://putnami.dev/install.ps1 | iex` also works, but the variable stays
  set in the PowerShell window: every later `irm | iex` of the installer in
  that window runs the command again. `?run=` and `--run` leave nothing set.

## What the installer trusts, and what it refuses

The rule is one sentence: **the installer installs bytes whose SHA-256 somebody
authoritative already stated, and refuses everything else.** The registry states
it on every successful download, in an `X-Integrity` header (or the `sha-256`
member of an RFC 9530 `Digest` header). The installer computes the SHA-256 of what
it received and compares.

`Integrity verified (sha256:…)` is printed on the success branch of that
comparison, and nowhere else. If you see that line, a comparison passed.

| Situation | What happens |
|---|---|
| Advertised digest matches the download | Installs. Prints `Integrity verified (sha256:<hex>)`. |
| Advertised digest does not match | Exits non-zero. `Integrity check failed: expected …, got …`. Nothing is installed. |
| No digest advertised | Exits non-zero, naming `PUTNAMI_UNSAFE_INSTALL=1`. |
| `PUTNAMI_UNSAFE_INSTALL=1` and no digest | Installs, with `Installing without integrity verification` on stderr. Never prints `verified`. |
| Neither `sha256sum` nor `shasum` on the host | Exits non-zero **before downloading**, naming both tools and the override. |
| `--download-url` without `--sha256` | Exits non-zero, naming `--sha256`. |
| `--sha256` disagrees with an advertised digest | Exits non-zero, printing both. |
| `--sha256` is not 64 hex characters | Exits non-zero, naming the expected form. |

An advertised digest is normalized before comparison exactly as
`extension.NormalizeIntegrity` does: the prefixes `sha256:`, `sha-256:`,
`sha256-`, and `sha-256-` are stripped, the rest is lowercased, and it must be 64
hex characters. Base64 SRI values and signatures are rejected, so there is one
canonical comparison path.

Two more refusals happen before anything lands in the install directory:

- **The version must be the one the registry resolved.** The installer runs the
  downloaded binary's `--version` and compares it with `X-Resolved-Version`,
  tolerating a leading `v` on either side. A mismatch is refused — a registry can
  resolve a fresh tag to a prior commit's artifact when release archives were not
  uploaded, and the resulting binary reports the older stamp. A
  binary that reports nothing, or `unknown`, is not blocked; that is the one
  direction where being lenient costs nothing.
- **The install directory must be writable**, checked with a real write probe
  rather than the mode bits, because a read-only mount, an ACL, or a full
  filesystem all pass `test -w` and then fail halfway through. The failure names
  the directory, the reason, and `--install-dir`.

### What "verified" does not mean

It means the bytes are the bytes the registry advertised, fetched over an
authenticated channel. It does not mean they were signed by Putnami. Artifact
signing is a separate mechanism and this installer does not check one.

### The registry must be reachable over an authenticated channel

Mirroring `extension.ValidateRegistryURL`:

| URL | Accepted |
|---|---|
| `https://…` | Always. |
| `http://localhost`, `http://127.0.0.1`, `http://[::1]` | Yes — the routine local-registry case. |
| `http://` anything else | Only with `PUTNAMI_ALLOW_INSECURE_REGISTRY=1`. |
| Any other scheme | Never. |

The check runs on `PUTNAMI_REGISTRY_URL`, or on `--download-url` when that is
given. With a command to run, it also runs on the command map URL. It runs
before the installer creates any directory.

A registry URL may carry credentials (`https://user:token@…`). Every line the
installer prints replaces that userinfo with `***@`, on the success path and on
every refusal, because the release smoke tails this log straight into CI output
where it would otherwise publish the token.

### The command map is trusted as far as the installer

The [command map](#the-command-map) is not signed. It is served from the same
origin as `install.sh`, over HTTPS, so it is trusted exactly as far as the
installer itself: whoever can change the map can already change the script you
pipe into bash. A second trust root for the map would add no protection the
script does not already bypass.

What the map decides is bounded:

- It names an extension; it cannot name a URL, a path, or a shell command. The
  installer refuses any value outside the `@scope/name[@constraint]` grammar and
  passes the value to the CLI as one argument.
- The CLI installs that extension through the registry, and verifies the
  archive against the SHA-256 digest the registry advertises, fail-closed, like
  any extension install. The map cannot make the CLI accept unverified bytes;
  only `PUTNAMI_UNSAFE_INSTALL=1` in your own environment does.
- A map at another URL (`PUTNAMI_COMMAND_MAP_URL`) is held to the same
  https-or-loopback rule as the registry, including the
  `PUTNAMI_ALLOW_INSECURE_REGISTRY=1` opt-in.

### The installer never escalates privileges

There is no `sudo`, `doas`, `pkexec`, or `run0` in the script, in any branch. It
writes to `~/.putnami/bin`, links only into directories that are both already on
your `PATH` and writable by you, and appends to your own shell startup file.

If there is no writable target, it says so and stops:

```
✗ Install directory /usr/local/bin is not writable by alex.
  Pick a writable location with --install-dir <dir>, or set PUTNAMI_INSTALL_DIR=<dir>.
  This installer never escalates privileges — run it as a user who can write to the target.
```

A system-wide install into a directory you do not own is therefore not something
this script can do for you. Run it as a user who owns the directory, or install
into `~/.putnami/bin` (the default) and link from there yourself.

## Supported platforms

`install.sh` installs `darwin`|`linux` × `amd64`|`arm64`, and `install.ps1`
installs `windows/amd64`. Together they cover the matrix of the
[First Public-Release Contract](../../../README.md#first-public-release-contract).
Anything else **exits non-zero** before
the prerequisite check and before any network call — a warning followed by an
install just moves the failure to the first `putnami` invocation, where the
warning is no longer on screen.

| Detected by `install.sh` | What happens |
|---|---|
| `Darwin` / `Linux` × `x86_64`, `amd64`, `arm64`, `aarch64` | Installs. |
| `CYGWIN*`, `MINGW*`, `MSYS*` | `install.sh does not install on Windows. Use the PowerShell installer:`, then the `install.ps1` one-liner to type in a PowerShell window, and for `cmd.exe` or the current shell a `curl.exe` download run with `powershell -File`. Installs nothing. |
| Any other `uname -s` | `Unsupported operating system: <value>`, plus the supported set. |
| Any other `uname -m` | `Unsupported architecture: <value>`, plus the supported set. |

| Detected by `install.ps1` | What happens |
|---|---|
| Windows, `AMD64` | Installs. |
| Windows, `ARM64`, `x86`, or any other architecture | `Unsupported architecture: <value>`, plus the Windows target. |
| Any other operating system | `install.ps1 installs the Putnami CLI on Windows only.`, then the `install.sh` one-liner. |

`install.ps1` reads the architecture of the machine, not of the PowerShell
process, so a 32-bit PowerShell on 64-bit Windows still installs `amd64`.

### The `amd64` → `x64` registry alias

The installer sends Go's `GOARCH` vocabulary, so `uname -m` is normalized to
`amd64` or `arm64` and sent as `?arch=amd64`. This is the same vocabulary
`putnami upgrade` sends (`runtime.GOARCH`) to the same endpoint.

The registry stores artifacts under `darwin-x64`, `linux-x64` and
`windows-x64`. Its download
handler normalizes the query first — `amd64` and `x86_64` both map to `x64`, and
`aarch64` maps to `arm64` — so `arch=amd64` resolves the `x64` artifact. If you
are reading published artifact names and wondering where `amd64` went: the alias
is the answer, and it is server-side. An unrecognized value is passed through and
then 404s, because no artifact matches that `os-arch` key.

## Flags and environment variables

Every flag has an environment equivalent. The flag wins.

| Flag | Environment | Default | What it does |
|---|---|---|---|
| `--version <tag>` | `PUTNAMI_VERSION` | `latest` | Channel (`latest`, `stable`, `canary`, a branch) or exact version. A bare semver is `v`-prefixed, matching `putnami version use`. |
| `--install-dir <dir>` | `PUTNAMI_INSTALL_DIR` | `~/.putnami/bin` | Install under the plain name `putnami` in this directory instead of the versioned layout. |
| `--variant <go\|ts>` | `PUTNAMI_VARIANT` | `go` | Names the binary `putnami-<variant>-<version>` in the versioned layout. |
| `--download-url <url>` | `PUTNAMI_DOWNLOAD_URL` | — | Fetch an explicit asset URL instead of resolving a channel. Requires `--sha256` unless the URL advertises a digest. |
| `--sha256 <hex>` | `PUTNAMI_EXPECTED_SHA256` | — | The SHA-256 you expect. Compared against the download, and against any advertised digest. |
| `--no-agent-hosts` | `PUTNAMI_NO_AGENT_HOSTS=1` | off | Skip Claude Code and Codex registration. |
| `--run <command>` | `PUTNAMI_RUN` | the `?run=` value, else none | After installing, pin the extension the command map names for `<command>` and run `putnami <command>` in the current directory. See [Run one command without a workspace](#run-one-command-without-a-workspace). |
| `--help`, `-h` | — | — | Print the usage block, including this contract in short form. |
| — | `PUTNAMI_REGISTRY_URL` | `https://put.putnami.dev` | Registry base. Validated as above. |
| — | `PUTNAMI_COMMAND_MAP_URL` | `https://putnami.dev/install-commands.txt` | The command map `--run` reads. Validated like the registry URL. |
| — | `PUTNAMI_ALLOW_INSECURE_REGISTRY=1` | off | Accept a plaintext non-loopback registry. |
| — | `PUTNAMI_UNSAFE_INSTALL=1` | off | Install without integrity verification. The only override. |
| — | `NO_COLOR` | off | Disable color. Color is already off when stdout is not a terminal. |

Unknown options exit non-zero and point at `--help`.

### Install layouts

Two layouts, chosen by whether `--install-dir` is given:

- **Default (versioned).** `~/.putnami/bin/putnami-<variant>-<tag>`, with
  `~/.putnami/bin/putnami` as a relative symlink to it. This is the layout
  `putnami version use` and `putnami upgrade --global` manage, so installs and
  upgrades land in the same shape and you can switch between them later.
- **`--install-dir <dir>`.** `<dir>/putnami`, plain name. Host registrations,
  when Claude Code or Codex is installed, still live in the host's user config;
  add `--no-agent-hosts` when that is not wanted. This layout is for callers
  that manage the binary directory themselves — package builds, images, and CI
  caches.

Both write through a staged `rename(2)`: the file is created beside its
destination and renamed over it, never rewritten through the existing inode.
Overwriting a running executable in place invalidates macOS's cached code
signature for that path, and every later `exec` of it is killed or hangs
uninterruptibly until reboot. The active symlink is swapped the same way, so a
concurrent `exec` sees the old target or the new one and never a missing file.

## Making `putnami` work in the shell you are in

The installer tries three things in order, then reports what actually resolves.

1. **The install directory is already on `PATH`.** Prints
   `<dir> is already on your PATH`. Nothing is written to a startup file.
2. **A user-writable directory already on `PATH`** — `~/.local/bin`, `~/bin`, or
   `/usr/local/bin`, first match wins — gets an atomically swapped symlink.
   A missing home-directory candidate is created first; a missing system
   directory never is. Prints `Linked <link> → <binary>`. No privilege
   escalation.
3. **Neither.** Prints the exact line to run now, and appends it once to the
   startup file your shell reads:

   ```
   ⚠  putnami is not on your PATH yet. Run this now, in this shell:
     export PATH="/home/alex/.putnami/bin:$PATH"
   ✓ Added it to /home/alex/.bashrc for future shells
   ```

   | `$SHELL` | File | Line |
   |---|---|---|
   | `zsh` | `${ZDOTDIR:-$HOME}/.zshrc` | `export PATH="<dir>:$PATH"` |
   | `bash` | `~/.bash_profile` if it exists, else `~/.bashrc` | `export PATH="<dir>:$PATH"` |
   | `fish` | `~/.config/fish/config.fish` | `fish_add_path <dir>` |
   | anything else | `~/.profile` | `export PATH="<dir>:$PATH"` |

   Startup files are only ever **appended** to, keyed on the line itself, so
   re-running the installer never duplicates it. The installer does not rewrite
   your dotfiles.

   The same line is the first command under `Next:` at the end of the output,
   before `putnami --help`. In the two other cases `Next:` carries no `PATH`
   line.

Whatever happened, the last thing the installer says about discovery is what
`putnami` resolves to right now, compared by inode so a versioned symlink counts:

```
✓ putnami now runs /home/alex/.local/bin/putnami
```

and, when something else shadows it:

```
⚠  putnami currently runs /usr/local/bin/putnami, not the build just installed (/home/alex/.putnami/bin/putnami).
  Put /home/alex/.putnami/bin earlier on your PATH, or remove the other copy.
```

The installer never says the command is available. It says what it did and what
the shell resolves — different sentences, both checkable.

### Shell completions

Completions are generated by the freshly installed binary and are never fatal:
the CLI works without them.

| `$SHELL` | File |
|---|---|
| `zsh` | `$ZSH_CUSTOM/completions/_putnami` under oh-my-zsh, else the first writable `$fpath` directory under `$HOME`, else `~/.zfunc/_putnami` |
| `bash` | `~/.local/share/bash-completion/completions/putnami` |
| `fish` | `~/.config/fish/completions/putnami.fish` |
| anything else | none generated |

Only the `~/.zfunc` fallback needs a line in `.zshrc`, and it is appended with
its own `compinit` rather than inserted before yours.

## Install on Windows

Open a PowerShell window and type, from an empty directory:

```powershell
irm https://putnami.dev/install.ps1 | iex
putnami init --project webapp --extension ts
putnami serve webapp
```

`irm` (`Invoke-RestMethod`) downloads the script and `iex`
(`Invoke-Expression`) runs it. From `cmd.exe` or Git Bash, download the script,
then run it as a file:

```
curl.exe -fsSLo install.ps1 https://putnami.dev/install.ps1
powershell -NoProfile -ExecutionPolicy Bypass -File install.ps1
```

`curl.exe` ships with Windows 10 version 1803 and later.

Do not pass the one-liner to PowerShell on a command line, as in
`powershell -c "irm <url>/install.ps1 | iex"`. Microsoft Defender blocks that
command line: on a Windows Server 2022 test host, it reported
`powershell.exe -Command "irm <url>/install.ps1 | iex"` as
`Trojan:Win32/Commando.A!ml` and stopped it. On the same host, Defender
reported nothing for `irm <url>/install.ps1 | iex` run as PowerShell script
text, nor for `powershell -NoProfile -ExecutionPolicy Bypass -File install.ps1`
run on the downloaded script.

### Prerequisites

| Prerequisite | What needs it | How to check it |
|---|---|---|
| Windows 10 version 1803 or later, or Windows 11, on `amd64` | The release builds `windows/amd64` only. Version 1803 is the first to ship `tar.exe`, which extracts the release archive. | `winver` |
| Windows PowerShell 5.1 or PowerShell 7 | `install.ps1`. | `$PSVersionTable.PSVersion` |
| `LongPathsEnabled` set to `1` | Store paths exceed 260 characters, and the Git, Bun and Go processes Putnami starts open them. Setting it needs an administrator once. | `putnami doctor` reports it and prints the command that sets it. |
| Git for Windows, with `core.longpaths` set to `true` | Workspace hooks run through `sh -c`, and `@putnami/contributor` skills run `sh`. Git for Windows provides `sh` in its `usr\bin` directory. | `putnami doctor` reports `core.longpaths`. A hook that finds no `sh` fails with an error that names Git for Windows. |
| Microsoft Visual C++ Redistributable (x64) | `@putnami/typescript` runs Biome to lint TypeScript and to format generated clients. The Windows build of Biome imports `vcruntime140.dll`, and Windows does not start it without that file (exit status `0xC0000135`). Install it with `winget install --id Microsoft.VCRedist.2015+.x64`. | `putnami doctor` reports it and prints the command that installs it. |
| Bun v1.4.0 or later | The TypeScript init/serve path. `@putnami/typescript` looks for `bun` on `PATH`, then where Bun's installer puts it: `%BUN_INSTALL%\bin`, by default `%USERPROFILE%\.bun\bin`. | `bun --version` |
| Docker Desktop | `putnami compose` only. | `docker version` |

The installer and the golden path also need the same network access as on
macOS and Linux: the public site and artifact registry, and the public package
registry (`https://npm.putnami.dev`) that `bun install` uses.

Workspaces check out with LF line endings. `putnami init` writes a
`.gitattributes` file with `* text=auto eol=lf` when the workspace has none,
and `putnami doctor` reports text files that Git checked out with CRLF.

### What `install.ps1` does

`install.ps1` follows the same trust model as `install.sh`. It verifies the
download against the digest the registry advertises, refuses what it cannot
verify, binds the install to the version the registry resolved, and never
elevates. The differences are Windows details:

- **Options.** Run as a file, it takes the flags in
  [Flags and environment variables](#flags-and-environment-variables), plus
  `--no-agent-hosts`. `irm | iex` passes the script no arguments, so set the
  environment variables instead:

  ```powershell
  $env:PUTNAMI_VERSION = 'canary'; irm https://putnami.dev/install.ps1 | iex
  ```

  To pass flags without saving the script, run it as a script block:

  ```powershell
  & ([scriptblock]::Create((irm https://putnami.dev/install.ps1))) --version canary
  ```

- **Transport.** Every download uses TLS 1.2 or later. The script raises the
  session's TLS setting to that floor only for its own requests: it keeps
  TLS 1.3 where the session or Windows already offers it, and it never lowers a
  stronger setting. For the same requests it clears any certificate validation
  callback the session set, so a callback that accepts every certificate does
  not apply, and it restores both settings afterwards. Each redirect the
  registry answers with must pass the same URL rule as the registry itself.
- **Assets.** The script accepts a `.tar.gz` archive that contains
  `putnami.exe`, or the raw executable. It refuses anything else, including a
  zip file. It extracts with the `tar.exe` that Windows ships, named by full
  path, so a GNU `tar` from Git for Windows earlier on `PATH` is never used.
- **Layout.** The default install writes
  `%USERPROFILE%\.putnami\bin\putnami-<variant>-<tag>.exe` and makes
  `putnami.exe` beside it a copy of that file, not a link. `--install-dir <dir>`
  writes `<dir>\putnami.exe`. `%USERPROFILE%\.putnami` is the Putnami home on
  Windows; it does not roam with the user profile.
- **Replacing a running CLI.** Windows refuses to delete or overwrite a running
  executable, but allows a rename. The installer holds a lock on
  `.putnami-switch.lock` in the install directory, renames the current file
  aside to a `.old-` name that starts with a dot, and moves the new one in. A
  later install, `putnami upgrade` or `putnami version use` deletes the file
  that was moved aside, once it no longer runs. All three follow the same lock
  and names. When the new file cannot move in, the old one moves back. When
  that fails too, the old one stays at its `.old-` path and the error names
  that path: move it back before the next install or switch in that directory,
  which deletes it.
- **`PATH`.** The installer puts the install directory first in your user
  `Path` value in `HKCU\Environment`, keeping its registry type and any
  unexpanded `%VARIABLES%`, and adds it to the current session. It needs no
  administrator rights. Terminals that were already open keep their old `PATH`,
  so the installer prints the line to run in each:

  ```
  Terminals that were already open keep their old PATH. In one of them, run:
    PowerShell:  $env:Path = "C:\Users\alex\.putnami\bin;$env:Path"
    cmd.exe:     set "PATH=C:\Users\alex\.putnami\bin;%PATH%"
  ```

  The last discovery line says which `putnami` resolves, as on Unix.
- **Completions.** The CLI has no PowerShell completion, so none is installed.
- **Agent hosts.** Claude Code and Codex registration is the same as on Unix,
  with the same arguments, messages and opt-out.
- **Failures.** A refusal ends the script with an error instead of `exit`, so an
  interactive PowerShell window stays open. `powershell -c` and `-File` runs
  exit with code 1. With a command to run, the script ends with that command's
  exit status: see [Run one command on Windows](#run-one-command-on-windows).

### What differs on Windows

These platform differences are decisions, recorded in
[ADR 0052](adr/0052-windows-consumers-junctions-job-objects-lf-and-long-paths.md):

- **Links.** Where Unix uses a directory symbolic link, Windows uses a
  junction, which needs no Developer Mode. Each link the CLI replaces keeps a
  `<link>.lock` file beside it.
- **Links it does not create.** On Windows, a symbolic link inside an extension
  archive or a runner source fails with an error that names the entry, and so
  does a junction inside a replaced Go module. A file link inside a replaced
  module needs Developer Mode, and the error says so.
- **Child processes.** Processes the CLI starts end with the CLI. On Unix, a
  child that outlives the CLI keeps running until the next run reaps it. A stop
  request sends `CTRL_BREAK_EVENT` and, after the command's grace period, ends
  the whole process tree.
- **`--kill-port`.** It kills nothing on Windows and prints a warning. Free the
  port yourself.

### Support status

- **Status**: `stable`, recorded as the `cli/windows-consumers` feature entry
  in the reviewed [`putnami.support.json`](../../../putnami.support.json).
- **Scope**: consumers who install published Putnami artifacts on
  `windows/amd64`, on Windows 10 version 1803 or later or on Windows 11
  ([D-W1](../decisions.json)), with the
  [prerequisites](#prerequisites) above: Git for Windows for `sh` (D-W3),
  `LongPathsEnabled` and Git `core.longpaths` (D-W8), LF checkouts (D-W4),
  and the Visual C++ runtime that Biome needs (D-W12).
- **Not covered**: `windows/arm64`, which the release does not build and
  `install.ps1` refuses. Contributors to this repository stay on macOS and
  Linux: `putnamiw` and the contributor gate do not run on Windows.
- **Evidence**: four test runs on a Windows Server 2022 host created for each
  run and deleted after it (D-W11). The last run had 0 product failures across
  145 Go test packages, and it passed the Go, TypeScript and junction-root
  consumer flows. The contributor gate runs `GOOS=windows go vet` on every
  `go.work` module (D-W11). The [Evidence](#evidence) table names the test for
  each Windows claim.
- **Not proven**: how Microsoft Defender treats the https forms of the one-line
  install. [Install on Windows](#install-on-windows) describes the forms that
  were tested, and on which host.

## The release smoke

[`tooling/cli/scripts/smoke-check-release.sh`](../scripts/smoke-check-release.sh)
runs the whole public golden path against a published channel. The repository's
maintainer CI intentionally has no schedule or operating-system matrix: per
[ADR 0011](adr/0011-release-governance-and-neutral-ci.md), an external release
execution plane is the automation boundary for those post-publication
invocations instead of adding a second GitHub Actions surface.

Any configured plane must fan the candidate channel out to five jobs before
promotion: the four actual Unix runners and one `windows/amd64` job. It also runs
the default public endpoint against `latest` nightly on the four Unix runners.
The `windows/amd64` job runs the PowerShell port of the same legs,
`smoke-check-release.ps1`, through `install.ps1`, on a Windows host that is
created for the run and deleted after it
([D-W11](adr/0052-windows-consumers-junctions-job-objects-lf-and-long-paths.md)).
That host does not exist between runs, so `latest` has no nightly Windows job:

| Invocation | Required actual runners | Blocks / escalation |
|---|---|---|
| `smoke-check-release.sh <candidate-channel>` | `darwin/amd64`, `darwin/arm64`, `linux/amd64`, `linux/arm64` | Blocks promotion; page the release owner with the failing leg and artifacts. |
| `smoke-check-release.ps1 <candidate-channel>` | `windows/amd64` | Blocks promotion, like the four Unix runners. |
| `smoke-check-release.sh latest` (default installer URL) | the four Unix runners | Nightly public monitor; page the release owner and stop the next release until triaged. |

The Windows host has no Docker, so the smoke does not run `putnami compose`
there.

When that external plane is not configured or is unavailable, the release owner
runs the same five candidate jobs manually before promotion and the same four
Unix `latest` jobs nightly until automation is configured or restored; one local
host is not a matrix substitute:

```bash
tooling/cli/scripts/smoke-check-release.sh canary
tooling/cli/scripts/smoke-check-release.sh              # public latest
```

It fetches `https://putnami.dev/install.sh` by default and selects the candidate
through `PUTNAMI_VERSION`, so the command piped to Bash has no private flags. It
runs against isolated home, XDG config/cache/data/state, Putnami store/artifact,
Bun/npm cache, Git config, and workspace directories, with credentials and proxy
variables cleared. It pins `SHELL` to the Bash that runs the gate so completion
and profile behavior cannot vary with the runner's login shell. No ambient state
can make a broken release look healthy.

The smoke hands the same channel to `init`: once it has cleared every
`PUTNAMI_` variable, it exports `PUTNAMI_CHANNEL=<channel>`. `init` then
resolves the extensions, the template and the starter's dependencies on the
candidate channel, so a candidate run uses the candidate's release set while
`latest` names an older one or nothing. The `init` command line stays the public
one, with no `--channel` flag, and a `latest` run sends the requests it sent
without the variable. The smoke does not rely on the channel the CLI was
installed from: on Windows the active `putnami.exe` is a copy whose name carries
no channel. The argument is a channel; `init` refuses an exact version. See
[ADR 0056](adr/0056-init-resolves-on-one-channel.md).

| Leg | What must hold | Regression guarded against |
|---|---|---|
| `channel` | `GET /putnami/cli/download` returns 200, an `X-Resolved-Version`, and a SHA-256 that describes the bytes streamed. | An earlier release skipped upload, or advertised a digest for the wrong bytes. |
| `install` | The public installer URL installs from that channel, reports `Integrity verified`, never takes the unverified path, and invokes no `sudo`/`doas`/`pkexec`/`run0`. | The installer escalated privileges, or reported "verified" without checking. |
| `discovery` | `command -v putnami` resolves, inside the smoke's workdir. | The installer escalated privileges, or reported "verified" without checking. |
| `prerequisites` + `platform` + `stamp` | The actual runner has Bun v1.4.0 or later; the installed path is executable, hashes to the selected host asset, executes on that runner, and reports the resolved version. | A stale cross-compile cache hit shipped a binary with the wrong embedded version stamp. |
| `init` | With `PUTNAMI_CHANNEL` set to the smoke's channel, the exact TypeScript command creates readable workspace/project state, extension/template locks, dependency lock/config, the assistant guidance block in `AGENTS.md`/`CLAUDE.md`, and a `.mcp.json` that registers the `putnami` MCP server. Root `.npmrc` maps `@putnami` to the public registry and contains no auth directive; no generated config/package manifest names the private cloud dependency; an installed-CLI build plan validates the generated graph. | Init reported success after a required setup step actually failed. |
| `serve` + `http` | The exact serve command emits canonical nested v2 typed readiness on an OS-assigned port and answers one non-empty explicit 2xx HTTP response. | Init reported success after a required setup step actually failed. |
| `shutdown` | The CLI and starter exit within the graceful budget without the hard fallback, and the ready URL becomes unreachable. | The CLI or starter failed to exit within the graceful budget. |
| `run` (only with `SMOKE_RUN_COMMAND`) | The run form installs, pins, and runs the command with exit status 0, escalates nothing, and leaves the directory it ran in untouched. | [The run scenario](#the-run-scenario) |

Every leg failure prints a bounded `::error::<leg> leg: …` diagnostic. The
server is terminated and reaped on success, failure, signal, and readiness
timeout. Set `SMOKE_DIAGNOSTICS_DIR` in an automated run to retain at most 200
lines per log, 400 lines per selected generated file, and a 400-entry generated
file list under `<channel>-<os>-<arch>`. Every retained artifact is also capped
at 256 KiB (262,144 bytes), so a single oversized line remains bounded; without
the opt-in the entire temp tree is still removed.

Two overrides exist so the smoke can be exercised without a release, and the
CLI's tests use them:

| Variable | Default | Purpose |
|---|---|---|
| `PUTNAMI_REGISTRY_URL` | `https://put.putnami.dev` | Point at a local registry. |
| `SMOKE_INSTALL_URL` | `https://putnami.dev/install.sh` | Point tests or a candidate site at another served installer. |
| `SMOKE_RETRIES` | 5 | Attempts before the channel leg fails (10s apart). |
| `SMOKE_STARTUP_TIMEOUT` | 180 | Seconds to wait for serve readiness. |
| `SMOKE_DIAGNOSTICS_DIR` | unset | Persist bounded failure evidence for CI upload. |
| `SMOKE_RUN_COMMAND` | unset | Add the `run` leg for this command. See [The run scenario](#the-run-scenario). |

### The run scenario

With `SMOKE_RUN_COMMAND=<command>`, the smoke adds a last leg that runs the
[run form](#run-one-command-without-a-workspace) of the public installer:

```bash
curl -fsSL "https://putnami.dev/install.sh?run=<command>" | bash
```

It runs in a new `git init` directory outside the starter workspace, with the
same isolated home and the same recording `sudo` on `PATH`. The leg passes when
all of these hold:

- The pipeline exits 0, which is the command's own exit status.
- The installer invoked no `sudo`, `doas`, `pkexec`, or `run0`.
- `git status --porcelain --untracked-files=all --ignored` reports nothing, and
  the directory still holds only `.git`. The listing catches an empty directory,
  which git does not report.

The command must be listed in the command map and must not wait for input. An
automated runner has no terminal, so the command's stdin is `/dev/null`. An
invalid name fails before the channel is fetched, and the leg needs `git` on
the runner.
With `SMOKE_INSTALL_URL` set, the leg reads the map served next to that
installer through `PUTNAMI_COMMAND_MAP_URL`, so a candidate site is tested with
its own map.

`smoke-check-release.ps1` has the same leg for `windows/amd64`. It writes the
[Windows run form](#run-one-command-on-windows) into a script, followed by
`exit $LASTEXITCODE`:

```powershell
irm "https://putnami.dev/install.ps1?run=<command>" | iex
exit $LASTEXITCODE
```

It runs that script with `powershell -File` in a new `git init` directory
outside the starter workspace, so the one-liner runs as script text and the
script's exit status is the command's. The leg holds the result to the same
three checks, with the smoke's recording `sudo` on `PATH`.

The release plane runs the smoke on darwin and linux: once the command map lists
a command, each of the four runners above (`darwin/amd64`, `darwin/arm64`,
`linux/amd64`, `linux/arm64`) sets `SMOKE_RUN_COMMAND` to a listed command that
reads no input, for the candidate gate and for the nightly `latest` monitor. A
manual run by the release owner does the same. A command can pass the leg only
once the release its entry resolves to runs without a workspace and is readable
by the smoke's anonymous installer, so the release owner turns the leg on for a
command after both hold.

## Rolling back

Rollback is always a **version pin**, never a compatibility flag — the same rule
[Compatibility and Migration](21-compatibility-and-migration.md#rolling-back)
states for artifact formats.

To install an older CLI on a machine:

```bash
curl -fsSL https://putnami.dev/install.sh | bash -s -- --version 1.2.3
```

On Windows, in a PowerShell window:

```powershell
$env:PUTNAMI_VERSION = '1.2.3'; irm https://putnami.dev/install.ps1 | iex
```

To make one workspace run an older CLI, without touching what is installed:

```bash
putnami pin 1.2.3        # run exactly this CLI in this workspace
putnami pin --remove     # drop the pin
```

`putnami pin` is never relaunched, so it works even when the pinned engine is
broken, and the pin records the binary's SHA-256 per `os/arch` and is verified
fail-closed on every launch — see
[Version Management](14-version-management.md#pinning-the-cli-per-workspace).

For a cold workspace pin, the Go launcher uses the configured native Put
endpoint and host-scoped authentication before verifying the downloaded digest.
The shell `putnamiw` download path still uses the `/dl` resolver.

Three rollbacks are **not** available, and all three absences are decisions:

- **There is no flag that restores best-effort integrity checking.**
  `PUTNAMI_UNSAFE_INSTALL=1` skips verification entirely and says so on stderr;
  it does not re-enable a mode where "verified" means "hashed". A downstream
  check that cannot trust the word has nothing to check.
- **There is no flag that restores privilege escalation.** An installer that can
  prompt for a password is an installer whose behavior depends on the sudoers
  file, and a rollback switch for it would have to be trusted by the same user
  who has not read the script. Install as a user who owns the directory, or link
  from `~/.putnami/bin` yourself.
- **There is no flag that downgrades a platform refusal to a warning.** The
  binary the registry would serve does not exist for that platform; installing it
  anyway produces `cannot execute binary file` later, with no evidence of why.

Rolling back an *installer itself* is a git operation, not a user-facing one:
both scripts are served from this repository through `sites/putnami.dev`, so
reverting the commit and republishing the site is the mechanism. The command
map is served the same way: removing an entry is a pull request against
`tooling/cli/scripts/install-commands.txt`, live when the site is republished.

## Evidence

| Claim | Where it is proven |
|---|---|
| A matching advertised digest installs, and prints the digest it compared | `internal/installscript/install_script_test.go` — `TestInstallVerifiesAdvertisedDigestAndVersionStamp` |
| An RFC 9530 `Digest` header is accepted when `X-Integrity` is absent | `TestInstallAcceptsRFC9530DigestHeader` |
| A mismatched digest refuses, installs nothing, and never prints `verified` | `TestInstallRefusesDigestMismatch` |
| No advertised digest refuses and names the override | `TestInstallRefusesWhenNoIntegrityIsAdvertised` |
| `PUTNAMI_UNSAFE_INSTALL=1` installs loudly and still never prints `verified` | `TestInstallWithoutIntegrityRequiresExplicitOptIn` |
| No SHA-256 tool refuses, before downloading | `TestInstallRefusesWithoutASHA256Tool` |
| A stale `--version` stamp refuses and leaves nothing behind | `TestInstallRefusesStaleVersionStamp` |
| `--download-url` requires `--sha256`, honors a matching one, and rejects a conflicting or malformed one | `TestDownloadURLRequiresAnExpectedDigest` |
| A non-writable install directory refuses before the download, naming the remedy | `TestInstallRefusesUnwritableInstallDirectoryBeforeDownloading` |
| A Windows shell gets the `install.ps1` one-liner and nothing installed; unknown `uname` values refuse, naming the detected value and the matrix | `TestInstallRefusesUnsupportedPlatforms` |
| `uname -m` maps onto `GOARCH` vocabulary | `TestInstallScriptSpeaksGOARCHVocabulary` |
| Plaintext non-loopback registries refuse; loopback and the opt-in do not | `TestInstallRefusesPlaintextNonLoopbackRegistry`, `TestInstallAcceptsPlaintextRegistryWithExplicitOptIn`, `TestInstallAcceptsLoopbackHTTPRegistryWithoutOptIn` |
| No branch escalates privileges, and the fake `sudo`/`doas`/`pkexec`/`run0` that prove it | `TestInstallScriptContainsNoPrivilegeEscalation`, `TestFakeEscalatorsRecordAnEscalationAttempt` |
| A successful install leaves no temporary directory behind | `TestInstallLeavesNoTemporaryDirectoryBehind` |
| Registry credentials never reach stdout, on success or on refusal | `TestInstallRedactsRegistryCredentials` |
| An unknown variant refuses, whether from `--variant` or `PUTNAMI_VARIANT` | `TestInstallRejectsUnknownVariant` |
| A failed completion regeneration keeps the completions that worked | `TestInstallKeepsWorkingCompletionsWhenRegenerationFails` |
| Claude Code and Codex receive the stable launcher without a fixed repository, and repeat installation adds it once | `internal/installscript/agent_hosts_test.go` — `TestInstallRegistersClaudeAndCodexWithStableSessionLaunchers` |
| Existing human host definitions, including TOML comments, are preserved byte-for-byte | `TestInstallPreservesExistingHumanAgentHostDefinitions` |
| Host-policy refusal warns without claiming host readiness or invalidating the CLI | `TestInstallDoesNotClaimHostReadinessWhenPolicyRejectsRegistration` |
| `--no-agent-hosts` and `PUTNAMI_NO_AGENT_HOSTS` skip every host call and still install the CLI | `TestInstallSkipsAgentHostsWhenDeclined` |
| A reformatted Codex answer is still recognized as Putnami's own definition | `TestInstallRecognizesAReformattedCodexDefinitionAsItsOwn` |
| The three discovery routes, missing user-path creation, and the idempotent profile append | `TestInstallLinksIntoAWritablePathDirectoryWithoutSudo`, `TestInstallCreatesMissingUserPathDirectoryForSameShellDiscovery`, `TestInstallPrintsAndRecordsThePathLineWhenNothingIsLinkable`, `TestInstallReportsAnInstallDirectoryAlreadyOnPath` |
| `Next:` starts with the `PATH` line when the shell does not reach the command, and carries none when it does | `TestInstallFooterStartsWithThePathLineOnlyWhenTheCommandIsNotReachable` |
| `README.md` and the getting-started page print the same first-use block, and it carries the `PATH` line | `sites/putnami.dev/test/docs-sources.test.ts` (`keeps the root and public getting-started commands identical`) |
| `--variant` and an exact `--version` still produce the versioned layout | `TestInstallHonorsVariantAndExactVersion` |
| The script runs when piped into `bash -s --` | `TestInstallRunsWhenPipedIntoBash` |
| Piped output is greppable — no ANSI escapes | `TestInstallOutputCarriesNoEscapeSequencesWhenPiped` |
| `--help` states the URL, the flags, the matrix, and the privilege promise | `TestHelpDocumentsTheContract` |
| The script advertises `putnami.dev/install.sh`, never `put.putnami.dev/install.sh` | `TestInstallScriptAdvertisesTheServedURL` |
| The smoke's install-log greps match strings the installer actually prints | `TestReleaseSmokeGrepsStringsTheInstallerPrints` |
| The smoke installs through `install.sh`, discovers the command, and checks the stamp | `internal/installscript/smoke_script_test.go` — `TestSmokeInstallsThroughTheInstallerAndDiscoversTheCommand` |
| The exact TypeScript init/serve argv, generated-state assertions, typed readiness, real HTTP response, and graceful stop all execute | `TestSmokeRunsExactTypeScriptGoldenPathThroughHTTPAndCleanStop` |
| Failure artifacts are opt-in and bounded | `TestSmokeCapturesOnlyBoundedOptInFailureDiagnostics` |
| The smoke hands its channel to `init` as `PUTNAMI_CHANNEL`, whatever the caller's environment holds, and keeps the public `init` command line | `TestSmokeHandsTheCandidateChannelToInit`, `TestSmokeScriptExportsTheChannelForInit`, `internal/installscript/smoke_ps1_test.go` — `TestSmokePS1HandsItsChannelToInit` |
| The smoke fails a release with no digest, a wrong digest, a stale stamp, a 404 channel, or an escalating installer | `TestSmokeFailsTheReleaseWhenNoDigestIsAdvertised`, `…WhenTheAdvertisedDigestDoesNotMatch`, `…OnAStaleStamp`, `…WhenTheChannelIs404`, `…WhenTheInstallerEscalates` |
| A binary that cannot report its version still fails the release *by name*, rather than killing the smoke silently through `pipefail` | `TestSmokeNamesTheStampLegWhenTheBinaryCannotReportItsVersion` |
| The smoke's `run` leg passes a command that leaves its directory untouched, and fails one that writes there, exits non-zero, or has an invalid name | `TestSmokeRunsTheOptionalRunScenarioInAnUntouchedDirectory`, `TestSmokeFailsTheRunScenarioWhenTheCommandWritesIntoTheCallerDirectory`, `TestSmokeFailsTheRunScenarioWhenTheCommandFails`, `TestSmokeRejectsAnInvalidRunCommandBeforeFetchingTheChannel` |
| The Windows smoke has the same `run` leg, with the same inputs, checks and error texts, and runs the one-liner as script text | `internal/installscript/smoke_ps1_test.go`; on Windows, `smoke_ps1_windows_test.go` — `TestWindowsSmokePS1RunsTheOptionalRunLegInAnUntouchedDirectory`, `TestWindowsSmokePS1FailsTheRunLegWhenTheCommandWritesIntoTheCallerDirectory`, `TestWindowsSmokePS1FailsTheRunLegWhenTheCommandFails`, `TestWindowsSmokePS1RejectsAnInvalidRunCommandBeforeAnyRequest` |
| The run form resolves, pins, then runs the command in the caller's directory: the command's output alone on stdout, the installer's on stderr, no footer, stdin never the pipe, the directory untouched | `internal/installscript/run_command_test.go` — `TestRunResolvesPinsAndRunsTheCommandInTheCallerDirectory`, `TestRunWorksWhenPipedIntoBash` |
| The run form exits with the command's status, and a failed pin exits with the pin's status without running the command | `TestRunExitsWithTheCommandsExitStatus`, `TestRunDoesNotRunTheCommandWhenThePinFails` |
| `?run=` sets the command in one placeholder line, and `--run` and `PUTNAMI_RUN` win over it | `TestInstallScriptCarriesExactlyOneRunPlaceholder`, `TestRunUsesTheCommandTheSiteBakedIn` |
| An invalid command name refuses before any network call, whether from `--run`, `PUTNAMI_RUN`, or the script | `TestRunRejectsAnInvalidCommandBeforeAnyNetworkCall` |
| A command the map does not list, and a map that is malformed, lists a command twice, is missing, or is announced as over 1 MiB, all refuse before the CLI is downloaded or installed | `TestRunRefusesACommandTheMapDoesNotList`, `TestRunFailsClosedOnAMalformedCommandMap` |
| The shipped map is well formed and empty | `TestShippedCommandMapIsWellFormedAndEmpty` |
| The map URL follows the registry's https-or-loopback rule | `TestRunRefusesAnInsecureCommandMapURL` |
| `--no-agent-hosts` and `PUTNAMI_NO_AGENT_HOSTS` still apply with a command to run | `TestRunHonorsNoAgentHosts` |
| The served `/install.sh` still contains the verification behavior | `sites/putnami.dev/test/app.test.ts` |
| The site serves `/install.sh` and `/install.ps1` byte-identical without `?run=`; for a valid `?run=` it changes only the placeholder line and keeps the content type and cache policy; it answers `400` without echoing any other value, and `500` for a script without exactly one placeholder | `sites/putnami.dev/test/app.test.ts` (`install.sh?run=<command>`, `install.ps1?run=<command>`), `sites/putnami.dev/test/installer-run.test.ts` |
| The site publishes the command map next to the installer, byte-identical, as a declared route | `sites/putnami.dev/test/app.test.ts` (`should serve the command map the installer reads for ?run=`), `sites/putnami.dev/test/http-routes.test.ts` |
| `install.ps1` hashes and compares the download before it extracts, runs or installs it; each of nine mutations of that check is caught | `internal/installscript/install_ps1_test.go` — `TestInstallPS1VerifiesTheDownloadBeforeUsingIt` |
| `install.ps1` runs no elevation command, the only registry value it sets is the user's `Path`, and it opens the machine `Path` read-only | `TestInstallPS1NeverEscalatesPrivileges` |
| `install.ps1` uses the lock file, locked byte and moved-aside names of `putnami upgrade` | `TestInstallPS1FollowsTheBinarySwitchContract` |
| When the new file cannot move in, `install.ps1` and `putnami upgrade` move the old one back; when that fails too, they keep it aside and name its path | `TestInstallPS1RestoresTheOldBinaryWhenTheMoveInFails`, `TestInstallPS1KeepsTheOldBinaryWhenARestoreFails`, `internal/commands/versioncmd/version_install_test.go` — `TestAsideSwitch_RestoresWhenMoveInFails`, `TestAsideSwitch_KeepsAndNamesTheOldBinaryWhenRestoreFails` |
| `install.ps1` is ASCII, runs in one scoped block, downloads over TLS 1.2 or later with no inherited certificate callback, takes every `install.sh` option, takes flags through the script block form, and advertises `putnami.dev/install.ps1` | `TestInstallPS1IsPlainASCII`, `TestInstallPS1RunsInOneScopedBlock`, `TestInstallPS1DownloadsOverTLS12OrLater`, `TestInstallPS1TakesTheOptionsOfInstallSh`, `TestInstallPS1TakesFlagsThroughTheScriptBlockForm`, `TestInstallPS1AdvertisesTheServedURL` |
| `install.ps1` installs the versioned layout after verifying, refuses a tampered, unadvertised, stale or executable-less download, and refuses unauthenticated registries and redirects | `TestInstallPS1InstallsTheVersionedLayoutAfterVerifying`, `TestInstallPS1RefusesATamperedDownload`, `TestInstallPS1RefusesAnUnadvertisedDigestUnlessUnsafe`, `TestInstallPS1RefusesAStaleVersionStamp`, `TestInstallPS1RefusesAnAssetWithoutTheExecutable`, `TestInstallPS1RefusesUnauthenticatedRegistries`, `TestInstallPS1FollowsRedirectsUnderTheRegistryRule` |
| `install.ps1` keeps the user's `Path`, refuses unsupported architectures and input before downloading, and registers agent hosts like `install.sh` | `TestInstallPS1KeepsTheUserPath`, `TestInstallPS1RefusesBeforeDownloading`, `TestInstallPS1RefusesUnsupportedInput`, `TestInstallPS1RegistersAgentHostsLikeInstallSh` |
| `install.ps1` carries one run placeholder, holds the command and map rules of `install.sh` with the same patterns, compared case-sensitively and byte for byte, resolves the command before it installs, pins with `extensions install --user --latest`, and calls `exit` only when it runs from a file; mutations of each check are caught | `internal/installscript/install_ps1_run_test.go` — `TestInstallPS1CarriesExactlyOneRunPlaceholder`, `TestInstallPS1HoldsTheRunRulesOfInstallSh`, `TestInstallPS1ResolvesTheRunBeforeInstalling`, `TestInstallPS1PinsTheExtensionLikeInstallSh`, `TestInstallPS1ExitsOnlyFromAFile` |
| Under `pwsh`, the run form pins, then runs the command in the caller's directory with only its output on stdout; it ends with the command's status, from a file through `exit` and from text in `$LASTEXITCODE`; a failed pin does not run the command; an invalid, unlisted or malformed input refuses before the CLI is downloaded | `TestInstallPS1RunResolvesPinsAndRunsTheCommandInTheCallerDirectory`, `TestInstallPS1RunEndsWithTheCommandsExitStatus`, `TestInstallPS1RunDoesNotRunTheCommandWhenThePinFails`, `TestInstallPS1RunUsesTheCommandTheSiteBakedIn`, `TestInstallPS1RunRefusesACommandTheMapDoesNotList`, `TestInstallPS1RunFailsClosedOnAMalformedCommandMap`, `TestInstallPS1RunRefusesAnInsecureCommandMapURL`, `TestInstallPS1RunRejectsAnInvalidCommandBeforeAnyNetworkCall`, `TestInstallPS1RunHonorsNoAgentHosts`, `TestInstallPS1BakedOneLinerRunsUnderPowerShell` |
| `install.ps1` installs through Windows PowerShell 5.1, through the one-liner, waits for the switch lock, and replaces a running `putnami.exe` | `internal/installscript/install_ps1_windows_test.go` |
| The run form works through Windows PowerShell 5.1 from a file, from the one-liner and the script block, and from `cmd.exe`; a failed pin and a refused input install nothing and leave the caller's directory empty | `install_ps1_windows_test.go` — `TestWindowsInstallPS1RunFromAFile`, `TestWindowsInstallPS1RunOneLiner`, `TestWindowsInstallPS1RunFromCmd`, `TestWindowsInstallPS1RunDoesNotRunTheCommandWhenThePinFails`, `TestWindowsInstallPS1RunRefusesBeforeInstalling` |
| The served `/install.ps1` is ASCII, still verifies what it downloads, and never escalates | `sites/putnami.dev/test/app.test.ts` |

The installer's tests drive the real `bash install.sh` against an `httptest`
registry serving a stub binary that answers `--version`. They skip when `bash` is
not on `PATH`, and every one of them asserts the installer never invoked the fake
`sudo` first on that `PATH`.

The `install.ps1` tests come in three kinds. Static tests read the script and
run on every host, so the contributor gate, which has no `pwsh`, checks each
contract: digest before use, refusals before the download, TLS floor and
certificate callback on every request, the binary switch and its failed
restore, and no elevation. The checks for the digest, the refusals, the
transport and the failed restore also run against mutated copies of the script
and must catch each one. Behavior tests run the script under
`pwsh` against the same kind of `httptest` registry, with the calls that read
Windows itself replaced by file-backed ones; they skip when `pwsh` is not on
`PATH`. The tests in
`install_ps1_windows_test.go` build only on Windows, run the real script
through `powershell.exe`, and need `PUTNAMI_INSTALLSCRIPT_WINDOWS_E2E=1`.
