# ADR 0108 — A cache hit reads its entry and writes only what differs

- **Status**: accepted
- **Date**: 2026-11-25
- **Extends**: [protocols/workspace ADR 0009](../../../../protocols/workspace/doc/adr/0009-a-files-stat-decides-that-it-is-unchanged.md)
  (a file's stat decides that it is unchanged) to the restore of a cache hit
- **Scope**: `@putnami/cli` (`internal/store`), the local task store
  (`~/.putnami/store`)

## Context

Once the digests of an unchanged run are answered from the file index, what
the engine still opens is mostly the store. Measured on 2026-11-25 on a
4-core Linux container, `strace -f` on the engine process (its children
excluded) of an unchanged `lint,test,build,validate --all --enforce-coverage`
run of this repository, 2,064 cache hits and 114 tasks that execute on every
run, at `4e94a26c`: 39,248 read-opens, of which 16,586 fell under
`.putnami/`:

- 8,256 opens of the four files of each hit's entry (`entry.json`,
  `result.json`, `meta.json`, `manifest.json`);
- 6,636 opens of the entries' output files, read to restore them;
- 456 lease files of the tasks that execute;
- 388 input-resolve records (ADR 0102), 375 invocation lease reads and 249
  command outputs of this run's verification;
- the rest: sessions, the file index, the workspace index, toolchain probes.

Each restore wrote its outputs again, so every output got a new inode and new
times, and the file index could not answer the digests the next run took of
them: the 17 generated `client.gen.go` files of this repository were read
about 20 times each, once per key that names them.

## Decision

1. **A restore writes nothing to a destination that already holds the
   output.** Before it stages anything, the restore compares the destination
   with the entry: for a file output, a regular file with the manifest's
   permission bits, size and digest; for a directory output, the tree of the
   entry's `files/` directory, every directory with its permission bits and
   every regular file with the manifest's, and nothing else outside the
   subtrees the output cedes. Permission bits compare with no setuid, setgid
   or sticky bit, which an entry never records; a directory that inherits
   setgid from its parent is therefore written again on each hit, at no cost
   to correctness. A destination file's digest is
   read through the file index, under its rule, and only once every stat
   agreed. The entry's files are listed and stated, never read; a file
   output's payload must be there, and an entry with bytes at a ceded path is
   left to the restore, which refuses it. Any difference, a link or another kind of file on either side, or a
   file the manifest does not list, makes the restore write the whole output as
   before. An unchanged destination keeps its stat, so the next run's digests
   of it are answered.
2. **What an unchanged run still opens in the store is the hit itself.**
   - **The entry.** A hit reads its descriptor, result, metadata and
     manifest once per run (one handle reads them once,
     `loadTaskEntryOnce`). They are the hit's answer: the result it replays,
     the outputs it declares and the digests decision 1 compares.
   - **The outputs a restore writes.** A restore that writes reads what it
     writes. On this repository they are `.gen` trees that two commands'
     generate tasks share, each restoring its own build stamp
     (`.gen/version.json`), so each run's restore of one undoes the other's.
   - **Leases**, of the tasks that execute; **invocation leases**, which say
     whether another process is alive; and the **command outputs** this run
     writes and its verification reads. Each is state of this run or of a live
     process, not a parse result.
   - **Input-resolve records**: one per answer, the record that keeps it
     (ADR 0102).

## Rejected alternatives

- **Keeping the entry's four files in the file index.** The store is shared
  by every worktree and every build of the CLI on the machine, and the index
  is one worktree's: each worktree would hold a copy of every entry it hits,
  2,064 entries of under 5 KB each here, about 10 MB, in a file every
  invocation loads and checksums, to save 8,256 opens of those same bytes.
- **One file per entry instead of four.** It would remove 6,192 of the 8,256
  entry opens measured above, and keep the hit's own read. The entry format is
  read by every CLI that shares the store, so it is a store protocol change,
  out of the scope of a CLI decision.
- **Restoring only the files that differ.** It would remove at most the 3,557
  output-file opens that remain at `efc5475e`, less those of the files that
  differ: on this repository a `.gen` tree differs by its build stamp alone. A
  directory output that cedes nothing is swapped whole, so a reader never sees
  a tree half old and half new; one that cedes subtrees is merged entry by
  entry (`MaterializeTaskOutput`). Decision 1 keeps both: it writes nothing,
  or what the restore wrote before.

## Consequences

Measured the same way at `efc5475e`, which applies decision 1: 34,865
read-opens (from 39,248); the entries' output files fell from 6,636 opens to
3,557, all of them restores that write, and no `.go` file is opened.

A destination a tool rewrote with the same bytes is left as the tool wrote
it. A destination whose permission bits or tree differ is replaced, as
before. A tool that compares the times of its outputs with its inputs, rather
than their bytes, may see an output older than an input whose bytes changed
back; it rebuilds, which costs time and never a wrong output.
