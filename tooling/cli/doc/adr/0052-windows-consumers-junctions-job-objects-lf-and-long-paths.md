# ADR 0052 — Windows consumers: junctions, Job Objects, LF checkouts, long paths and one home

- **Status**: accepted
- **Scope**: `@putnami/cli` (`tooling/cli`), `putnami-extension-sdk`
  (`dirlink`, `proctree`, `filelock`), `@putnami/go`, `@putnami/typescript`
  and `@putnami/python` on native `windows/amd64`

## Context

This record serves consumers: people who install published Putnami artifacts
and run them. A consumer on Windows 10 version 1803 or later, or Windows 11,
installs the CLI with one PowerShell line, then runs
`putnami init --project webapp --extension ts` and `putnami serve webapp`.
Contributors, `putnamiw` and the contributor gate stay on macOS and Linux.
The specification is
[`../22-installing-the-cli.md`](../22-installing-the-cli.md#install-on-windows)
and spec `specs/windows-consumers.json`.

Five Unix assumptions fail on native Windows:

- A directory symbolic link needs Developer Mode for a standard user, and
  `putnami install` creates extension links in every workspace.
- Process groups and signals do not exist, so a stop that signals and waits
  hangs.
- Git can check text out with CRLF, which changes every content digest.
- Store paths exceed 260 characters (the longest measured is 264). Go handles
  long paths itself; its Git, Bun and Go children need the Windows setting.
- `HOME` and `os.UserHomeDir` resolve to different directories.

The Windows decisions are D-W1 to D-W12 in `tooling/cli/decisions.json`. This
record gives the reasons for D-W4, D-W6, D-W7, D-W8, D-W9 and D-W12, and owns
the Windows installer.

## Decision

### 1. Directory links are junctions (D-W6)

A junction is a Windows directory link that a standard user can create without
Developer Mode. It stores the absolute path of its target.

Every directory link the CLI writes goes through
`go.putnami.dev/sdk/extension/dirlink`: extension, template and agent-artifact
stable links, `.putnami/sessions/latest`, the legacy `out/` link, and runtime
staging. On Unix it makes symbolic links. On Windows:

- `Create` makes a junction; a relative target resolves against the link's
  directory first.
- `Replace` removes the old link and creates the new one under an exclusive lock
  on `<link>.lock`. It never renames over a link. The lock file stays, because
  deleting a file a waiter may hold is unsafe. Readers do not take the lock, so
  a reader can briefly find no link.
- `filepath.EvalSymlinks` does not follow a junction. A reader that resolves a
  directory to walk it, compare two spellings of it or contain a path in it uses
  `dirlink.Resolve`. cli-model may require only protocol modules, so
  `workspace.ResolveLinks` is its standard-library copy, and a CLI test pins the
  two to one answer. A candidate toolchain executable keeps
  `filepath.EvalSymlinks`: it is a file, and resolving a Windows app execution
  alias by opening it fails.

Links from outside the CLI are refused by name, not skipped:

- On Windows, a symbolic link in an extension archive fails the install. The
  archive is extracted to staging and renamed, so a junction would point into
  the deleted staging directory.
- On every platform, a hard link or symbolic link that leaves the extracted
  tree fails the install, and so does an entry whose name has a `..` component,
  is absolute or UNC, or starts with a drive. A backslash is a separator on
  every platform.
- A symbolic link in a runner source snapshot is refused; a Windows runner is
  outside D-W1.
- A junction inside a replaced Go module fails staging: a staged copy cannot
  reproduce its absolute target. A file link inside one still needs the link
  privilege; when Windows refuses it, the error names Developer Mode and the
  link.

### 2. Process trees run in a Job Object (D-W9)

A Job Object groups processes. With `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`,
closing its last handle ends every process in it.

Every site that starts a child tree uses `go.putnami.dev/sdk/extension/proctree`:
the task runner, hooks, cache and runner provider sessions, session reporting,
`compose`, and the serve and run commands of the Go, TypeScript and Python
extensions.

- On Unix, a tree is the child's process group.
- On Windows, the child starts suspended with `CREATE_NEW_PROCESS_GROUP`, joins
  a new job, then resumes. Only a process started with `StartDetached` breaks
  away.
- A graceful stop sends `CTRL_BREAK_EVENT` to the tree's console process group,
  then terminates the job after the site's grace period. When the request
  cannot be sent, the tree is killed at once.
- Each site keeps its grace period. The Go extension's `serve` and `run` kill
  the tree 3 seconds after a stop request on every OS, under the runner's
  5-second force kill. Python keeps 5 seconds.

On Windows, children end with the CLI that started them, so no orphan server
holds a port. On Unix, a child that outlives the CLI keeps running and the next
run reaps it. `--kill-port` relies on `lsof` or `fuser`; on Windows it kills
nothing and prints a warning.

### 3. Checkouts use LF (D-W4)

`putnami init` writes a `.gitattributes` with `* text=auto eol=lf` when the
workspace has none, and never changes an existing one, even under `--force`.
`putnami doctor` reports tracked text files checked out with CRLF, and
`core.autocrlf=true` in a workspace without an LF rule; the fix it prints is the
rule. The content hasher never normalizes line endings: different bytes must
not share a key.

### 4. Long paths are prerequisites (D-W8)

`LongPathsEnabled` in `HKLM\SYSTEM\CurrentControlSet\Control\FileSystem` and
Git's `core.longpaths` are prerequisites on Windows. `putnami doctor` reports
each one that is off or unreadable and prints the command that enables it. The
store layout is not shortened.

### 5. The Visual C++ runtime is a prerequisite (D-W12)

The Windows build of Biome, which `@putnami/typescript` runs to lint and to
format generated clients, imports `vcruntime140.dll`, and Windows refuses to
start it without the Microsoft Visual C++ Redistributable. `putnami doctor`
checks `%SystemRoot%\System32\vcruntime140.dll` and names the Redistributable
when it is missing. Putnami neither bundles nor installs it: installing needs
administrator rights, and the installer never elevates.

### 6. One home (D-W7)

Every component resolves the Putnami home with `os.UserHomeDir`: on Windows,
`%USERPROFILE%\.putnami`. That directory does not roam. The store holds
machine-local executables and caches, and a roaming profile would copy them at
every sign-in and sign-out.

### 7. The Windows installer

- `install.sh` run from Git Bash, MSYS2 or Cygwin installs nothing. It prints
  the `irm https://putnami.dev/install.ps1 | iex` line for PowerShell, and for
  cmd.exe or the current shell a download of `install.ps1` run with
  `powershell -File` (Microsoft Defender blocks the one-liner passed on a
  command line). It exits non-zero.
- `install.ps1` installs `windows/amd64` only, under the trust model of
  [ADR 0012](0012-install-trust-and-release-smoke.md) §1 and §2: it verifies the
  download against the advertised digest, refuses what it cannot verify, and
  never elevates. The only registry value it sets is the user's `Path` in
  `HKCU\Environment`; it opens the machine `Path` read-only.
- `putnami.exe` is a copy of the versioned binary, not a link. Windows refuses
  to overwrite a running executable but allows a rename, so `install.ps1`,
  `putnami upgrade` and `putnami version use` switch the same way: under a lock
  on `.putnami-switch.lock`, the current file moves aside, the new one moves in,
  and a later switch deletes the aside file. When the new file cannot move in,
  the old one moves back; when that fails, the old one stays aside and the error
  names its path.
- The release smoke has a `windows/amd64` candidate job on a Windows host
  created for the run and deleted after it (D-W11). The nightly `latest`
  monitor stays on the four macOS and Linux targets.

The other decisions, D-W1 (target), D-W2 (`.tar.gz` archives with
`compiled/<name>.exe`, the CLI's own raw executables excepted), D-W3 (hooks through `sh -c` from Git for Windows), D-W5
(OS-class cache-key field on Windows only), D-W10 (`LockFileEx` locks) and D-W11
(the proof host), need no further reason here.

## Enforceable invariants

| Invariant | Test |
|---|---|
| A junction is created, replaced and read as a standard user | `tooling/extension-sdk/dirlink/dirlink_windows_test.go` |
| A directory reached through a link is read as that directory | `TestRuntimeRootBehindADirectoryLinkIsReadAsItsDirectory`, `TestRuntimeReplacementBehindADirectoryLinkIsDigested`, `TestTaskOutputLockIdsAgreeAcrossADirectoryLinkedRoot`, `TestRequireInsideWorkspaceFollowsDirectoryLinks`, `TestSameDirPathFollowsADirectoryLink`, `TestRelaunchSourceWorkspaceResolvesADirectoryLinkedRoot`, `TestCommonDir_DirectoryLinkedCheckoutAgrees` |
| Removing a link never deletes its target | `TestRemoveDeletesOnlyTheLink` |
| A three-level tree ends as a whole | `tooling/extension-sdk/proctree/tree_windows_test.go` |
| A failed stop request kills at once; a tree past its grace is killed | `tooling/extension-sdk/proctree/stop_test.go` |
| An archive link and a replaced-module junction are refused by name | `TestExtractTarGzRefusesContainedSymlinksOnWindows`, `TestRuntimeReplacementJunctionIsRefusedByDigestAndStaging` |
| `init` writes the LF rule and keeps an existing `.gitattributes` | `TestWorkspaceInitWritesTheLFPolicy`, `TestWorkspaceInitKeepsAnExistingGitattributes` |
| `doctor` names CRLF files and each long-path setting with its fix | `TestCheckWorkstation_CRLFFilesAreNamedWithTheFix`, `TestCheckWorkstation_LongPathPrerequisitesAreNamedWithTheirFix` |
| A hook without `sh` names Git for Windows | `TestShellNotFound_NamesGitForWindows` |
| `install.ps1` verifies before it installs and never elevates | `TestInstallPS1VerifiesTheDownloadBeforeUsingIt`, `TestInstallPS1NeverEscalatesPrivileges` |
| A running CLI is replaced by a rename aside | `TestVersionUpdate_ReplacesRunningCLIByRenameAside` |
| A failed restore keeps the old binary aside and names it | `TestAsideSwitch_KeepsAndNamesTheOldBinaryWhenRestoreFails`, `TestInstallPS1KeepsTheOldBinaryWhenARestoreFails` |

Tests under `*_windows_test.go` build only for Windows and run on the Windows
host of the release smoke.

## Consequences

- A standard Windows user installs and runs Putnami without Developer Mode,
  administrator rights or WSL2.
- Every link the CLI replaces on Windows leaves a `<link>.lock` beside it.
- An extension whose archive contains a symbolic link does not install on
  Windows until its publisher ships a regular file or directory.
- Stopping `putnami serve` or closing its terminal on Windows ends the whole
  server tree.
- Without `LongPathsEnabled`, Putnami works only while no path exceeds 260
  characters, and `doctor` says so before the first failure.

## Rejected alternatives

- **Symbolic links with Developer Mode.** Every consumer would change a system
  setting, and managed machines often forbid it.
- **Hard links for the CLI binary.** A hard link to a running executable cannot
  be replaced, which is the case `upgrade` exists for.
- **Junctions for archive links.** Staging-then-rename leaves them pointing at a
  deleted directory; copying the target changes what extraction means.
- **A no-op lock or a no-op stop on Windows.** Garbage collection could delete
  what another worktree writes, and a stop could hang.
- **Normalizing line endings in the hasher.** Different bytes would share a key.
- **A shorter store layout.** It changes every store path on every platform.
- **`%LOCALAPPDATA%` as the home.** It adds a second home rule and moves the
  store away from where every other platform and document puts it.
