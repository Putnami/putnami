# Testing at Production Boundaries

A test that guards a production boundary answers with a recorded exchange, not
an authored one. An incident that reached `main` or production closes with a
test that replays it and fails on the tree before its fix.

A production boundary is any place where the CLI meets something it does not
build: the Put registry over HTTP, the `putnami cloud registry-token` credential
command, the environment a hosted run exports to every job, and the shape of the
session a hosted run starts.

## Why authored fixtures are not enough

An authored fixture answers the way its author believes the other side answers.
When the belief is wrong, the test is green and the product is not.

Two cases show the failure:

- A stub models a CLI without the cloud extension as
  `echo 'Unknown command: cloud'; exit 2`. A real core-only CLI prints an
  undeclared-flag notice and `no project matched --projects "registry-token"`.
  Code that classifies the first line of that output tells every core-only
  install that the cloud "cannot write the credential yet". A recording exposes
  the error the first time it replaces the stub.
- Registry fixtures that assume a laptop's environment break on a publishing CI
  job. That job exports an invocation broker that wins over every fixture, so
  the tests go red on `main` and never on pull requests.

## Rule 1: record the exchange

Use `go.putnami.dev/sdk/extension/recorded`.

| Boundary | Record with | Replay with |
| --- | --- | --- |
| HTTP response | `curl -si --http1.1 '<url>' > <what>.<status>.http` | `recorded.HTTP` and `recorded.NewServer` |
| Command | `<command> > <dir>/stdout 2> <dir>/stderr; echo $? > <dir>/exit` | `recorded.Command` and `recorded.Executable` |

`recorded.NewServer(t, next, responses...)` answers the first request with the
first response, and so on. After the last response it hands every request to
`next`, or repeats the last response when `next` is nil. A recorded 502
followed by a handler that serves an archive is a retry test.

`recorded.Executable(t, exchange, branches...)` writes a program that prints the
recorded bytes and exits with the recorded status. A `recorded.Branch` replays a
different exchange when an environment variable holds a value. Use a branch
only for a condition the real command has, such as the first-use install that
`PUTNAMI_NO_AUTO_INSTALL=1` turns off.

### Where recordings live

| Recordings | Directory |
| --- | --- |
| Put registry responses | `tooling/cli/testdata/recorded/put-registry/` |
| `registry-token` exchanges | `tooling/extension-sdk/registrycred/testdata/recorded/registry-token/` |

Each directory has a `README.md` with one row per recording: the request or
command that produced it, the date, and every redaction.

### Redact without changing the shape

A recording never holds a live credential or personal data. Replace a secret
with a value of the same length and structure. The registry bearer is an RS256
JWT of 1011 bytes in three segments of 123, 544 and 342 bytes, valid for
8 hours. The redacted bearer keeps those sizes and that lifetime and replaces
every claim value, the key id and the signature. A parser then meets the size it
meets in production.

### Declare recordings as test inputs

The Go extension keys `test~test` on `**/*.go`, `go.mod` and `go.work`. A
recording outside those patterns does not change the task key, so a changed
recording replays the old verdict. Add the directory to the project's
`options.test.filePatterns`, as `tooling/cli/putnami.json` and
`tooling/extension-sdk/putnami.json` do.

### What stays authored

- A success body the test builds, such as an archive whose digest the test
  checks. Record the refusal; build the success.
- A sentinel handler that fails the test if it is ever called.
- A fake that records the argv or environment it receives, when that input is
  what the test asserts.
- A shape nothing can produce today, such as a cloud that predates a flag. Say
  so in the test's comment.

A recording can be partly reconstructed when the source kept only part of it.
The 502 gateway reset is one: the run log kept the status line and the body, not
the headers, and not the host. Its `README.md` says which part is
reconstructed. Replace it with a full recording when the fault recurs.

## Rule 2: an incident closes with its fixture

1. Put the test in the package that owns the broken behavior. Name the test
   and its file after the behavior it guards, not after an issue or a pull
   request.
2. Replay what production did: the recorded response, the recorded command
   output, the environment the host exported, or the session shape the host
   ran.
3. In the test's comment, state the contract the test holds, not its history.
4. Prove it red once, by hand, not in CI:

   ```bash
   git worktree add --detach /tmp/pre-fix <parent-of-the-fix>
   # copy the test, its helpers and its recordings into /tmp/pre-fix
   cd /tmp/pre-fix
   ./putnamiw test --projects <project> --run '^<TestName>$' --no-cache --coverage=false
   ```

5. Run it green on the fixed tree, then remove the worktree.

When the host's environment caused the incident, re-run the test binary
(`os.Args[0]`) with that environment and one existing test selected. The child
process gets exactly what a hosted job gets, and the parent asserts on the
child's result.

## Incidents replayed

| Test | Incident | Replays |
| --- | --- | --- |
| `TestCredentialChildAfterALockChangeReturnsTheBearer` (`registrycred`) | `registry-token` prints setup lines before the bearer after a lock change | The stdout a shim captured from the real command |
| `TestFixtureDownloadsIgnoreAHostedRunsBroker` (`clibin`) | A publishing run's broker answers 401 to test fixtures on `main` | The broker variable, answered with the registry's recorded 401 |
| `TestCLIPinDownloadSurvivesAGatewayResetBeforeHeaders` (`clibin`) | A 502 on an archive upload fails a whole CI run | That 502 on the CLI pin download, then the archive |
| `TestAPublishingPushRunPlansEveryProjectsTests` (`engine`) | A library's red Postgres suite runs on pull requests but never on `main` | The push run's session: gate and `publish --channel` with every project selected |
