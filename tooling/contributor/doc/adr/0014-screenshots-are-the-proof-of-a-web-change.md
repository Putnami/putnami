# ADR 0014 — Screenshots are the proof of a web change

- **Status**: accepted
- **Scope**: `@putnami/contributor`: a new `change screenshots` command,
  `change finalize`, and the `build` skill's prove and check stages
- **Extends**: [ADR 0005](0005-the-contributor-executable-serves-the-change-noun.md),
  on the `change` command group, and
  [ADR 0009](0009-one-contribution-loop-from-talk-to-merge.md), on what the
  prove stage produces

## Context

An agent that changes a web project never sees what it rendered. Lint, tests,
build and qualification pass, and a screen that a person finds hard to read
ships: nothing in the loop asks for a picture of the result. The prove stage
already says to capture the screens a change touches, but no command does it,
so the step is skipped or improvised, and `change finalize` cannot tell.

A headless Chromium is usually present (a hosted session ships one), and every
web workload commits its route inventory, `schema/http-routes.json`, whose
`file-route` GET routes are its pages and whose `evidencePath` names each page
file.

## Decision

1. **`putnami change screenshots` captures the touched pages.** It reads the
   project's committed route inventory at the base and the head, and the paths
   between the merge base and the head. A page file in the diff touches its
   page; another changed file under the project's `src/` touches every page,
   unless it is the evidence of a route, such as an action or an API handler,
   which renders no page.
   Each touched page is captured at its first route, or at the routes the run
   names, with template values from a fixture file: desktop and mobile, light
   and dark, before and after. The PNG files and a manifest bound to the head
   commit, with the sha256 of each file, go to an output directory under
   `.putnami/`, outside Git.
2. **The agent serves; the command captures.** The executable never starts
   putnami again (the host channel is its only way into the core). The agent
   serves the head, and the base from a worktree of the merge base, and passes
   both URLs. The command refuses a project directory with uncommitted changes.
3. **The browser is driven over the DevTools pipe, from Go.** The command
   starts Chromium with `--remote-debugging-pipe` and speaks the DevTools
   protocol on the two inherited descriptors with the standard library. No
   Node runtime or Playwright package is needed. Sign-in reads a Playwright
   `storageState` file, the format people already record.
4. **`change finalize` asks for them.** A ready publication of a change that
   touches a page of a web project needs, for each such project, a manifest of
   the published head that holds the after captures of every touched page,
   with each file intact. `--waive-screenshots <reason>` replaces them for one
   change, and the reason is written into the verification line. A draft, and
   a change that touches no page, need none.
5. **The agent reviews its own screens.** The prove stage has the agent read
   each capture against the workspace's design rules and fix what it finds
   before the final gate. The check stage hands the manifest and the files to
   the independent reviewer with the diff.

## Consequences

- A web change is ready only with pictures of its head, or with a reason the
  person can read on the proposal.
- The captures stay local. Uploading them as run evidence, and showing them on
  a hosted change page, needs a hosted evidence member that does not exist yet
  and belongs to the hosted platform.
- A shared component or layout change captures every page of the project; a
  project with many page files produces many files. `--route` narrows nothing
  the rule selects; it adds routes.
- A page served only at runtime, absent from the route inventory, is not
  selected.

## Alternatives rejected

- **Run a Playwright script.** It needs the Playwright package resolvable in
  every workspace, and a Node runtime the contributor executable does not
  otherwise need.
- **Let the command serve the base and the head.** It would start putnami from
  the executable, which the host channel design forbids, and duplicate the
  composition `qualify` owns.
- **A policy switch for the requirement.** The waiver is per change and leaves
  a reason; a switch would silence the requirement for every change.
