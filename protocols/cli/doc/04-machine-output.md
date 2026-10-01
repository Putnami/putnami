# Bounded machine output

The bounded machine-output profile makes a live `--output=jsonl` stream safe to
capture in CI and agent contexts without losing the final verdict or silently
discarding failure evidence. It is an additive profile on the version-2 session
stream: a legacy `SessionStreamRecord` remains valid without `machineOutput`,
while `ValidateSessionStream` / `validateSessionStream` require the distinct
bounded final shape.

This document specifies the contract. The Putnami CLI producer adopted it: it
emits the bounded final record, uses the same sanitized record bytes for live
selection and persistence, and verifies the whole live/artifact pair with
sequence validation. Other producers conform only when they do the same.

## Fixed live budgets

Every byte count is the UTF-8 length of compact JSON plus one LF per record. The
total includes the mandatory final `session:end` record.

| Mode | Total | Failure reserve | Final reserve | Derived ordinary partition |
|------|-------|-----------------|---------------|----------------------------|
| `normal` | 1 MiB / 1024 records | 256 KiB / 256 records | 16 KiB / 1 record | 770048 bytes / 767 records |
| `verbose` | 8 MiB / 8192 records | 2 MiB / 2048 records | 16 KiB / 1 record | 6275072 bytes / 6143 records |

These values are contract constants, not producer settings. `normal` excludes
debug task detail from the live copy; `verbose` admits that detail under its
larger fixed budget. Neither mode can change task selection,
execution, cache behavior, task or run verdicts, or the process exit code.

The three partitions are hard. Ordinary records cannot borrow unused failure
capacity, and neither ordinary nor failure records can borrow the final reserve.
Unused capacity is not reclaimed. This permits a deterministic online decision:
sanitize and compact each arriving record, classify it, then apply the mode's
detail policy and fixed partition. Normal-mode debug detail is elided before it
can spend ordinary capacity; every other whole record is admitted only if both
byte and record capacity remain in its partition. Records are never truncated,
and admitted records retain arrival order.

Failure priority is deliberately narrow and stable:

- `task:end` whose status is `failed` or `canceled`;
- `task:event` whose `level` is `error`;
- a `diagnostic` event whose `severity` is `error`;
- a `phase` event whose `status` is `failed`;
- a runtime-wire `result` event whose `data.status` is `FAILED`, or its
  documented CLI-normalized projection whose root `status` is `FAILED`.
- the single runtime-v1 successful managed-publication `result` whose
  `data.status` is `OK` and whose `data.data.releaseSet` carries the immutable
  deployment handoff. It is produced only at session finalization, so this
  protected path keeps it live after ordinary detail has exhausted its budget.

A `test:case` record is never failure priority, whatever its status. The failed
task's `task:end` already takes the reserve.

Everything else consumes the ordinary partition.

Debug task detail is:

- a `task:event` whose `level` is `debug`;
- a `test:case`, whatever its `status`.

Failure priority is evaluated before the detail policy. A future or malformed
event that is both failure-priority and `level: "debug"` therefore still uses
the protected failure reserve. Otherwise, debug task detail is retained only in
the complete artifact for `normal` and is eligible for the ordinary partition in
`verbose`. This is what keeps per-test success transcripts and test cases out
of default machine output without losing them for investigation. A failed
`test:case` is debug detail too: one failed task can report up to 1,000 of
them, which would spend the whole ordinary partition and elide later tasks'
records. The failure stays live through the failed `task:end` and the task's
error diagnostics.

## Complete artifact and exact elision

Selection limits only the live copy. The complete sanitized sequence is retained
as `events.jsonl` inside the session named by `machineOutput.artifact.sessionId`
and is pruned with that session (`retention: "session"`). The artifact path is a
fixed relative constant, never an arbitrary producer path.

If the CLI cannot create the session artifact before execution, JSONL falls
back to the complete unbounded v2 stream and its legacy `session:end` shape
(without `machineOutput`) rather than naming an artifact that does not exist.
Task selection, verdicts and exit status are unchanged. A persistence failure
after recording begins is reported separately; its terminal record likewise
makes no bounded-artifact claim.

The final record reports elision separately for `ordinary` and `failure`.
Normal-mode debug-policy omissions count as ordinary elision. Each class carries
exact whole-record and JSON-plus-LF byte counts; the two numbers are either both
zero or both positive. Whole-sequence validation recomputes the deterministic
selection from the artifact, requires an identical final line in both copies,
and compares every counter.

## Bounded terminal verdict

The opt-in final record uses `StreamRunSummary`, not the general `RunSummary`.
It cannot carry `failures` or `publications`, whose arrays are unbounded.
`counts.failed` remains exact, failure-priority detail gets the dedicated live
reserve, and complete detail remains in the artifact. The terminal line itself
must fit the fixed 16 KiB / one-record reserve.

This distinction preserves rollout compatibility: legacy per-line records keep
`RunSummary`, while `BoundedSessionEndRecord` makes the bounded promise explicit.
A legacy final is valid under `ValidateDocument` but rejected by whole-sequence
validation because it does not make that promise.

## Sanitization v1

`terminal-safe-redacted-v1` runs before persistence, measurement, and live
selection. It transforms string **values**; object member names are retained.
Member names are normalized only for sensitive-name lookup by lowercasing ASCII
and removing `-`, `_`, and `.`.

For every string value, sanitization:

1. makes UTF-8 valid and replaces isolated Unicode surrogates with U+FFFD (raw
   JSONL containing an isolated surrogate escape is rejected before decoding so
   Go and JavaScript cannot interpret it differently);
2. removes ECMA-48 terminal sequences, including CSI and OSC families;
3. replaces remaining C0/C1 controls with U+FFFD, except tab, LF, and CR;
4. replaces sensitive-member values with `[REDACTED]`;
5. redacts high-confidence private-key, AWS, GitHub, Google, Slack, and Stripe
   credential forms found inside otherwise ordinary strings.

The sensitive member vocabulary is closed for this sanitizer version:
`authorization`, `proxyauthorization`, `cookie`, `setcookie`, `token`,
`accesstoken`, `refreshtoken`, `password`, `secret`, `apikey`, `clientsecret`,
`privatekey`, `secretkey`, `credential`, and `credentials` after normalization.
Changing this behavior requires a new sanitization version rather than silently
changing the meaning of an artifact already labeled v1.

## Validation

`ValidateDocument(DocumentSessionStreamRecord, line)` and its TypeScript twin
validate one line. They preserve the legacy shape and enforce bounded terminal
rules only when `machineOutput` is present.

`ValidateSessionStream(live, artifact)` and `validateSessionStream(live,
artifact)` validate the whole bounded profile: compact framing, sanitization,
mandatory bounded final record, total and partition budgets, deterministic
selection, exact split elision, final-line identity, and the retained artifact
reference. The shared conformance corpus exercises the same sequences in Go and
TypeScript.
