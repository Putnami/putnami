# Parity with the surface this extension replaced

The acceptance for the extraction was never "the extension works". It was that
`@putnami/sdd` writes **the bytes the built-in command wrote**, for every one of
the fourteen subcommands and four MCP tools. This page says how that is proven
and what a reviewer has to know before touching the proof.

## Two phases: a live oracle, then a recording

**While core still had its implementation** (commits for Tasks 6 and 7), parity
was measured against a live oracle. One fixture workspace, every subcommand run
twice — once through the CLI's `App.runStructuredCommand`, once through the real
interactive extension path with a freshly compiled binary — and stdout, stderr
and the exit code compared byte for byte, with **zero per-subcommand
normalization**. The MCP tools were compared the same way, through one real
`mcp.Server` carrying both implementations, registered by real discovery.

**Deleting core deletes the oracle.** "The oracle is gone, so the test goes" is
the regression, not the cleanup — so the answers were recorded first, from
core's own implementation, at the parent of the removal commit, in the same
tree:

| Recording | Cases |
|---|---|
| `testdata/sdd-parity/structured` | 38 structured invocations |
| `testdata/sdd-parity/human` | 15 human invocations |
| `testdata/sdd-parity/mcp` | 20 MCP calls; the `list_features` ones hold the paged answer of [04-mcp-tools.md](04-mcp-tools.md) |
| `testdata/sdd-parity/contracts` | 4 generated contract artifacts |

They live with the tests that read them, in
`tooling/cli/internal/cli/testdata/sdd-parity`.

## What makes the recording evidence rather than decoration

Three properties, and all three are load-bearing:

1. **The capture pins git dates**, so the fixture repository's `HEAD` is stable
   across runs. Each answer's `head` field is then compared **verbatim** rather
   than normalized away — that field is the one thing proving which tree the
   answer describes, so normalizing it would make the comparison pass over the
   wrong workspace.
2. **A one-byte tamper fails the test.** Asserted, not assumed.
3. **There is deliberately no `-update` flag.** The only implementation that
   could rewrite these bytes is the one under test, so an update flag would let
   a drift be recorded as the new truth by the code that drifted. Regenerating
   the fixtures means checking out the pre-removal commit and re-capturing from
   core — expensive on purpose.

### Recorded answers that have been edited since the capture

An answer changes only by a human editing the file, in a change that says which
user-visible output moved and why. That has happened once:

| Date | Recordings | What changed |
|---|---|---|
| 2026-09-01 | `structured/architecture-snapshot.txt`, `structured/architecture-inspect.txt` | A required snapshot collection that is empty now renders as `[]` instead of `null`. The published schema (`putnami-architecture-snapshot.json`) requires `owns`, `exports`, `imports`, and `findings` to be arrays; the protocol's canonical writer emitted JSON `null` for an empty one, so the snapshot violated its own schema. The fix is in `protocols/architecture`, and it moves the built-in's answer exactly as it moves the extension's — the extraction is not what changed. |
| 2026-09-01 | every `structured/architecture-*.txt` | The detector reads framework evidence, so the snapshot gains `evidence` and the coverage report gains `domainAccess`. Both are constants for a workspace with no capability manifests — an empty array and `not-detected` — so the edit is one line per file, derived from the rule rather than copied from the implementation. |

`TestRecordedParityAnswersCoverEveryCaseAndNothingElse` fails in **both**
directions: a case that disappears from the suite fails just as loudly as one
that appears without a fixture. An acceptance that can shrink while every test
still passes is not an acceptance.

The contracts recording goes one step further. The report envelope only *names*
the files a generation produced, so a renderer that drifted inside them used to
pass; the fixtures now restore core's generated artifacts and a separate test
compares those four files byte for byte.

## The one production adaptation, and why it is not a fudge

A built-in structured command's payload is **captured** as JSON by the CLI and
decoded into `map[string]any` before it is re-encoded, so its keys come out
alphabetical. A Go struct encodes in declaration order. Without reproducing that
round trip, every success envelope would differ in key order alone.

`capturedPayload` reproduces it for a **success** envelope. A **failure**
envelope must not be round-tripped, because core passes the attached value
straight through. The asymmetry is core's, not a choice made here — and it
inverts on the MCP wire, where nothing round-trips at all (see
[04-mcp-tools.md](04-mcp-tools.md#parity-with-the-tools-they-replace)).

## Subcommands added after the capture

A subcommand that did not exist when core was removed has **no recorded oracle**,
and cannot be given one: there is no built-in implementation whose bytes it could
match. Its acceptance is its own tests, not a comparison.

| Subcommand | Acceptance |
|---|---|
| `architecture init` | `cmd/putnami-sdd/architecture_authoring_test.go` — the scaffold document, and the four refusals |
| `architecture sync` | same file — the mechanical reconciliation, and the refusal to grant an undeclared relation |

`TestSDDSubcommandsMatchTheRecordedBuiltIn` deliberately does not enumerate the
manifest, so a post-capture subcommand does not read as a missing recording.

## What parity does not cover

Four differences belong to the CLI's dispatcher rather than to this extension
and cannot be reproduced from inside a subprocess the CLI never reaches. They
are listed in
[03-commands.md](03-commands.md#where-the-dispatcher-answers-instead-of-the-extension).

## The manifest has one author

The committed `putnami.extension.json` must be exactly the document
`authoredManifest` in `manifest_contract_test.go` produces through
`go.putnami.dev/sdk/extension/manifest`. Edit the Go authoring code and the JSON
together; the test compares both in the protocol's canonical form and fails on
any difference. `TestHarnessRejectsAHandEditedManifest` proves that comparison
is not vacuous.

Two members are set outside the builder, and the test says so where it does it:
`runtime` (the SDK builder has no method for the runtime lifecycle primitive
yet) and `cliContract`.
