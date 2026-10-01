# Prior-release lock artifacts

These files are **recovered bytes**, not generated ones. Each is a
`putnami.lock.json` exactly as it was committed to this repository at the commit
that shipped (or first exercised) its lock format version.

They exist because the compatibility promise in
[`../../../../doc/21-compatibility-and-migration.md`](../../../../doc/21-compatibility-and-migration.md)
is about files users already have on disk. Regenerating an old fixture with
today's writer would make every reader test pass by construction: the writer and
the reader would agree because they are the same release. A recovered file
cannot agree by construction — it either loads or it does not.

## Checking a fixture

[`provenance.json`](./provenance.json) records, for every file, the source
commit, the source path, the commit date, the declared format
version, the SHA-256 of the bytes, and why that file is the representative one
for its version. Any reviewer can check a fixture against its record:

```bash
shasum -a 256 <file>   # must equal sha256
```

The fixtures recorded so far come from commits that predate the public 0.2.0
root, so `git show <sourceCommit>:<sourcePath>` does not find them in this
repository. A fixture recovered from a later commit can also be re-derived that
way.

## The immutability rule

`TestPriorReleaseFixturesAreImmutable` (`../../prior_release_test.go`) fails when
a fixture's bytes change, when a file is added without a provenance record, or
when a record names a file that is gone.

That guard exists to make one specific mistake loud: when a reader change breaks
a fixture, the cheap fix is to "update the fixture" and the correct fix is to
decide whether the reader just broke a lock file somebody has committed. A
fixture edited to match a new reader is a compatibility break with the evidence
deleted.

If a format change genuinely retires a version — the window's floor moves — the
fixture stays and its **expected verdict** moves from "loads" to "rejected with
the documented remedy". The corpus records the history; the budget records what
this build does with it.

## Recorded gaps

`provenance.json` also carries a `gaps` array: shapes the compatibility budget
covers for which this repository has no recoverable artifact (today: lock format
v4 and any lock carrying a `cli` pin). Naming a gap is the point — a corpus that
silently omitted them would read as complete.
