# #1450 cold and warm admission: measurements

Measurements behind the 2026-11-25 amendment of
`tooling/cli/doc/adr/0100-admission-charges-the-cpu-it-grants.md`.

- **A** = `main` at `00810ec6`. **B** = the option under test, one commit per
  series (`run.json` names it). One git worktree each (`/home/user/wt-A`,
  `/home/user/wt-B`), each running the CLI its own `./putnamiw` built from its
  own tree; B's worktree was checked out at each option's commit, and its CLI
  built, before the series started.
- Machine: 4-CPU Linux container (Intel Xeon @ 2.10 GHz), no CPU quota,
  14.3 GB memory limit. Nothing else ran during the runs.
- Every run ran alone and started once the one-minute load fell below the gate:
  **2.0** for the selection, **1.0** for the full gate. `/proc/loadavg` was
  sampled every 15 s from just before the start to the end (`load.txt`); load
  statistics are over those samples, pre-start sample included.

## Protocol

The protocol of `evidence/1450-remeasure` (branch `epic-925/1450-remeasure`):

- **Selection** (`campaign.sh <tag> <B-commit>`, `sel.env`): `./putnamiw
  lint,test,build,validate --projects <14 projects> --no-cache
  --enforce-coverage`. `sel-cold-*`: empty task history, A and B alternating,
  3 each. `sel-seed-*`: one run each from an empty history, whose
  `tasks-after.json` (`history/<tag>-sel-{A,B}-tasks.json.gz`) is restored
  before each `sel-warm-*` run, A and B alternating, 2 each.
- **Full gate** (`full.sh`, `full2.sh`, `final.sh`, `final2.sh`): the same
  command with `--all`, empty task history, `--no-cache`. One warm-up run per
  worktree first (`full-warmup-*`, cache on, not compared).

Per run, `runs/<label>/` holds `run.json` (commit, mode, session, start, end,
exit code, arguments), `report.json`, `session.json.gz`, `load.txt`,
`tasks-after.json.gz` and the last 40 lines of the log. `summarize.py`
produces `summary.md`; `tools/` holds the timeline, ready-queue and charge
readers used for the diagnosis.

## Series

| tag | B commit | B option |
|---|---|---|
| v1 | `ef43639f` | occupancy charge; a cold task granted the free CPUs, at least one |
| v2 | `d9a3a9cf` | v1, and a task with history granted at least the free CPUs |
| v3 | `513a352c` | v2, and a waiter starting once a whole CPU is free |
| v4 | `ef274862` | v2, and a waiter starting while any CPU is free |
| full-cold-B1 | `ef274862` | v4 on the full gate |
| full-v5 | `663852ea`, `83d5a749`, `bd512797` | v4, a cold task granted at least half the machine |
| v6, full-v6 | `d9ab7157` | chosen: v4 for a task with history, a cold task keeps the whole machine |

`full-v5-cold-B2` and `-B4` stopped after 46 s on a requirement text over its
512-byte limit, fixed in `bd512797`; they measure nothing. `dbg-warm-B`
ran an instrumented build of `513a352c` that logged admission waits; it is
not part of any series.

## Results (medians)

- **Chosen, selection**: cold 69.2 s [69.2–75.0], load 4.0, against A 68.2 s
  [68.0–72.0], 3.9; warm 63.8 s [62.8–64.8], load 3.4, against A 83.6 s
  [81.6–85.6], 2.8. Warm is faster than cold.
- **Chosen, full gate**: `full-v6-cold-B7` 541.0 s, load 13.6, against
  `full-cold-A6` 527.4 s, 14.6; A over six runs 514.2 to 558.3 s.
  `full-v6-cold-B6`, 623.2 s, was the first run of B's tree: the Go build
  cache, which `--no-cache` does not reset, was cold for the changed CLI
  packages (`/tooling/cli` lint 61.6 s against 3.3 s).
- **Rejected cold rules, full gate**: free CPUs at least one, 826.4 s, load
  5.2, failed (`/tooling/cli` tests at one CPU); at least half the machine,
  605.6 s, load 9.3. Both first runs of their trees.

`summary.md` has every run and every series.
