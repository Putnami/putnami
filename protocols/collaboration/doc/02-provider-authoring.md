# Writing a collaboration provider

A provider is an ordinary Putnami extension whose manifest declares one tool
per operation it implements and marks each with `putnami.dev/provider`. It
needs no service, daemon or new loader: Putnami discovers it like any
extension, starts its process only when a call arrives, writes one request to
its stdin and reads one result from its stdout.

The reference implementation is
[`@putnami/local-collaboration`](../../../tooling/local-collaboration/README.md)
(`tooling/local-collaboration`), which implements tasks and proposals over a
file store in about 600 lines of Go. The excerpts below come from it.

## 1. Declare the operations

One tool per operation. The tool name is yours (namespace it with your
extension's name); callers never see it, because Putnami exposes the operation
under the contract's name.

```json
"local-collaboration.tasks.update": {
  "description": "Change a local task's title, body or labels. expectedRevision is compared and written under the store's exclusive lock; a change that alters nothing keeps the revision.",
  "inputSchema": { "type": "object", "description": "The tasks.update request document of go.putnami.dev/protocol/collaboration version 1." },
  "annotations": { "readOnlyHint": false, "destructiveHint": true, "idempotentHint": false, "openWorldHint": false },
  "_meta": {
    "putnami.dev/contract": { "access": "mutating", "readOnly": false, "supportsDryRun": false },
    "putnami.dev/provider": { "contract": "tasks", "version": 1, "operation": "update", "preconditions": "atomic" }
  },
  "command": "{extensionRuntime}",
  "args": ["provider-tool"],
  "cwd": "{workspaceRoot}",
  "timeoutMs": 30000
}
```

The rules `ReadProviderOffer` enforces, and the orchestrator applies when it
resolves a binding:

- `readOnlyHint` and `_meta["putnami.dev/contract"].access` agree with the
  operation's access ([operations](01-operations.md)); a read is never
  destructive, and an operation marked destructive there has
  `destructiveHint: true`. `supportsDryRun` is false: the contracts have no
  dry-run member.
- `preconditions` is present exactly for the operations that take a revision:
  `atomic` if your backend compares and writes in one step, `checked` if you
  compare then write (a concurrent writer can interleave), `none` if you cannot
  compare at all — Putnami then refuses a request carrying `expectedRevision`
  instead of letting you drop it. `claim` and `memory.checkpoint` require
  `atomic`.
- `workspaceSelection: true` on `memory.context` and `memory.search`, which
  take `projects`, `impacted` and `baseline`: Putnami resolves them and puts
  the resolved `selection` and the workspace membership on your request. Never
  resolve a selector yourself.
- Every required operation of a version you declare, and one tool per
  operation. Any error withdraws that contract version from your offer, so a
  workspace binding it fails clearly rather than half-working.
- `description` says what your backend does and does not do. It is what
  `<contract> capabilities` shows beside each operation: the local provider
  says it runs no hosted checks and offers no merge.
- `openWorldHint` is true when the operation reaches a system outside the
  machine.

Hold your manifest to both gates in a test (`cmd/putnami-local-collaboration/main_test.go`):

```go
manifest, diags := extension.ParseManifest(data)            // strict extension protocol
diags = extension.ValidateManifest(manifest)
offer, diags := collab.ReadProviderOffer(manifest.Tools)    // collaboration declarations: expect none
```

## 2. Answer one call

A routed call's `ToolCallRequest` names your tool and carries a `provider`
member: the contract, version and operation the workspace bound, and the
binding's `settings`. `arguments` is the request document, already validated
and normalized by Putnami (a list request always has an explicit page).
`collaboration.Serve` validates it again, dispatches on the operation, checks
your result against the contract, and writes one `ToolCallResult` whose text is
a `Response`:

```go
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	switch {
	case len(args) == 2 && args[0] == "__putnami" && args[1] == "runtime-info":
		// the runtime handshake: a runtimeproto.Info naming this extension
	case len(args) == 1 && args[0] == "provider-tool":
		if err := collab.Serve(context.Background(), stdin, stdout, provider.New().Handlers()); err != nil {
			return 1
		}
		return 0
	}
	return 2
}
```

Handlers are keyed by `collab.OperationKey{Contract, Version, Operation}` and
return a result document or a `*collab.Failure`:

```go
func (p *Provider) getTask(_ context.Context, call collab.Call) (any, *collab.Failure) {
	ref := call.Input.(*collab.TaskRefInput).Ref
	s, _, failure := storeOf(call)            // settings → store directory
	if failure != nil {
		return nil, failure
	}
	state, failure := readState(s)
	if failure != nil {
		return nil, failure
	}
	index := taskIndex(state, ref)            // a reference from another source is not ours
	if index < 0 {
		return nil, collab.Fail(collab.OutcomeNotFound, "task.missing", "no task %s in source %s", ref.ID, ref.Source)
	}
	return &collab.TaskResult{Task: state.Tasks[index]}, nil
}
```

`Serve` turns a handler that panics, or returns a result the contract refuses,
into `unresolved` for a mutation and `unavailable` for a read, and turns a nil
page into `[]`.

## 3. Get the semantics right

- **References.** Issue references whose `source` names your backend instance
  (`local:<store id>`, `github:<owner>/<repo>`), so two stores never collide,
  and answer `not_found` for a reference another source issued.
- **Idempotency.** For `tasks.create`, `proposals.review` and
  `memory.checkpoint`, record the key with a digest of what it asked for, in
  the same atomic step as the write. A repeat with the same content returns the
  first result (`created: false` / `replayed: true`); other content is a
  `conflict` with reason `idempotency.mismatch`. This is what makes a caller's
  reconciliation of an uncertain write safe.
- **Exact identity.** `proposals.upsert` keeps at most one open proposal per
  exact repository/base/head, found and written in one atomic step. Answer
  exactly what was asked: Putnami refuses a result about another reference,
  another head, or a state other than the requested transition.
- **Preconditions.** Compare `expectedRevision` with the current revision and
  answer `conflict` with `error.current` when they differ. The local provider
  does both under an exclusive `flock` on its store, so `atomic` is true; a
  hosted backend without conditional writes declares `checked` and says so.
  A revision changes whenever anything the item reports changes. The one
  exception is a task's `parent`: a provider that leaves it outside the
  revision says so in its `link` tool's description, as the GitHub provider
  does.
- **Proposal labels and assignees.** Report them on every answer about a
  proposal, including those your own settings add, and `[]` when there are
  none; or never, when your backend has no such thing. Absent means "not
  reported", so a caller that verifies them can say it did not. The local
  provider has none and answers `[]`.
- **Uncertain writes.** If your backend call may have succeeded but you cannot
  tell, answer `unresolved` and never retry it blindly yourself. Its
  `reconcile` hint names the read or idempotent repeat that settles it as
  `<contract>.<operation>` (`read the task with tasks.get`); a hint that names
  no operation of your contract is refused. Putnami
  already reports a mutation whose process timed out, was canceled or crashed
  as `unresolved`.
- **Pages.** Return at most `page.size` items in a stable order and an opaque
  `page.next` cursor; omit it on the last page. The cursor names the position
  after the last item returned, never a page number, so the next request may
  ask for another size. `Serve` asks a list handler again with a smaller size
  when a page would exceed the 4 MiB document bound, and returns that shorter
  page. A provider in another language shortens the page itself: the CLI
  refuses a larger answer and tells the caller to ask for a smaller
  `page.size`.
- **Disclosures.** Report what you do not have. The local provider answers
  `proposals.status` with `checks: {"state": "unsupported", "detail": …}` and
  declares no `merge` tool, so `capabilities` lists merge as unsupported.

## 4. Keep credentials yours

Resolve credentials inside the provider (its environment, the host keyring,
an existing CLI login). Never ask for them in `settings`: a credential-named
setting invalidates the binding, and so does a setting value that is a URL
carrying a password, or any user under `http`/`https`
(`https://x-access-token:…@github.com/…`); an SSH account without a password
(`ssh://git@host/…`) is not a credential. Settings are committed and echoed by
capability discovery. Putnami never forwards your stderr to a caller, and redacts the
values of credential-named environment variables and URL userinfo from your
messages and results — do not rely on that: keep secrets out of what you
write.

## 5. Test it

- Run the shared contract scenarios: build a `providertest.Target` from your
  handlers and settings and call `providertest.RunTasks`,
  `providertest.RunProposals` or `providertest.RunMemory`
  ([`providertest`](../providertest/providertest.go)). They drive the
  handlers through `collab.Serve` and check idempotent replays, stale
  revisions, exact proposal identity and pages against any backend, including
  a shared one. The local provider runs the tasks and proposals scenarios
  against a fresh store, and
  [`@putnami/github-collaboration`](../../../tooling/github-collaboration/README.md)
  against an in-memory GitHub stand-in and, on request, a real repository.
- A memory target also names `Workspace`, the name the orchestrator fills
  into an identity, and `Reopen`, which returns a second provider instance on
  the same store. `RunMemory` checks creation with `mustNotExist`, the replay
  of a repeated idempotency key, `revision.conflict` for a stale
  `expectedRevision` and `record.exists` for a second creation (both with
  `current`), resume from the second instance, that of concurrent writers at
  one revision exactly one lands, pages, that another workspace sees
  nothing, and that the largest checkpoint the contract accepts
  (`providertest.MaximalCheckpoint`) is read back unchanged, so a store's own
  size bound can never be below the contract's. [`@putnami/memory-store`](../../../tooling/memory-store/README.md)
  runs it against each of its backends.
- Drive your handlers through `collab.Serve` with the request a routed call
  carries (`tooling/local-collaboration/internal/provider/provider_test.go`),
  so every answer passes the same validation a real call does.
- Race concurrent writers against one store: every create gets its own
  identifier, and of concurrent updates against one revision exactly one wins.
- `tooling/cli/internal/cli/collaboration_local_provider_test.go` builds the
  local provider, binds it in a temporary workspace and calls it through the
  command line and the MCP server against one store.
