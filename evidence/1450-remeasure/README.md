# #1450 remeasure: the scheduler admits the CPU it grants

Independent before/after measurement of PR #1490 (#1450), merged to `main` as
`740e905d` on top of `a36f193d`. No product code changed.

- **A** = `a36f193d` (main before #1490), **B** = `740e905d` (main after).
- One git worktree per commit, each running the CLI its own `./putnamiw`
  built from its own tree (distinct content keys).
- Machine: 4-CPU Linux container (Intel Xeon @ 2.10 GHz), cgroup-v1, no CPU
  quota, 14.3 GB memory limit. Nothing else ran during the runs.
- Every run ran alone, A and B alternating, and started only once the
  one-minute load fell below the gate: **1.0** for the full-gate runs,
  **2.0** for the selection runs (relaxed on request to fit the time budget).
  `/proc/loadavg` was sampled every 15 s from just before the start to the
  end (`runs/<label>/load.txt`). Load statistics are over those samples;
  p90 is the sample at rank floor(0.9 n). They include the pre-start sample.

## Protocol as run

1. **Full gate** `./putnamiw lint,test,build,validate --all --no-cache --enforce-coverage`,
   `.putnami/stats/tasks.json` removed before each run (`campaign.sh`):
   one warm-up run per worktree (fresh worktree: installs and toolchains,
   not part of the comparison), then `cold-A1`, `cold-B1`. The planned
   A, B, A, B cold series, the warm-history series and the unchanged-workspace
   series were **stopped after `cold-B1`** on request: a full gate takes
   12 min here (23 min for a fresh worktree), so the plan would have taken
   about 5 h.
2. **Project selection** (`selection.sh`), the same gate restricted with
   `--projects` to 14 projects:
   `/go/framework/{client,api,database,app,openapi,grpc,telemetry}`,
   `/typescript/framework/{client,application,web,analytics,cli-protocol,ui}`,
   `/tooling/cli-documents`. Its `--no-cache --dry-run` plan holds 528 jobs
   (build 238, test 176, validate 61, lint 42, validate-workspace 11) over
   75 projects (14 selected, 61 dependency builds); a run reports 524 tasks,
   364 executed and 160 skipped.
   - `sel-cold-*`: no cache, empty task history, A, B alternating, 3 each.
   - `sel-seed-*`: one run per commit from an empty history; its
     `tasks.json` (300 entries, `history/sel-{A,B}-tasks.json`) is restored
     before each `sel-warm-*` run.
   - `sel-warm-*`: no cache, that commit's seeded history, A, B
     alternating, 2 each.

Files: `run.sh` (one run), `campaign.sh`, `selection.sh`, `summarize.py`
(produces the tables below). Per run, `runs/<label>/` holds `run.json`
(commit, mode, session id, start/end, exit code, arguments), `report.json`
(`.putnami/reports/<id>.json`), `session.json`
(`.putnami/sessions/<id>/session.json`), `load.txt`, `tasks-after.json`
and the last 40 lines of the log.

## Per run

Duration is `run.durationMs`; run CPU and orchestration CPU are
`run.orchestration.cpu.runMs` and `.totalMs`; `peakGrantedCpus` and
`peakLoadCenti` come from `run.orchestration.capacity`, which only B reports.

| run | commit | session | start (UTC) | outcome | duration s | run CPU s | orch. CPU s | peakGrantedCpus | peakLoadCenti | load1 mean / p90 / peak (n) |
|---|---|---|---|---|---:|---:|---:|---:|---:|---|
| warmup-A | a36f193d | `20261125-125307-b48325` | 12:51:49 | success (exit 0) | 1416.0 | 5414 | 343 | – | – | 17.5 / 26.6 / 31.9 (101) |
| warmup-B | 740e905d | `20261125-131949-231935` | 13:19:24 | success (exit 0) | 1305.8 | 5015 | 335 | 16 | 2902 | 17.6 / 25.3 / 29.0 (90) |
| cold-A1 | a36f193d | `20261125-134418-688aa6` | 13:44:11 | success (exit 0) | 735.7 | 2755 | 345 | – | – | 15.4 / 22.1 / 26.0 (51) |
| cold-B1 | 740e905d | `20261125-135907-5a520c` | 13:59:00 | success (exit 0) | 701.7 | 2604 | 329 | 16 | 2800 | 15.7 / 23.1 / 27.0 (49) |
| sel-cold-A1 | a36f193d | `20261125-141234-7efe42` | 14:12:31 | success (exit 0) | 88.7 | 292 | 57 | – | – | 4.7 / 6.6 / 6.6 (8) |
| sel-cold-B1 | 740e905d | `20261125-141516-5ad51f` | 14:15:13 | success (exit 0) | 85.3 | 277 | 54 | 16 | 632 | 4.4 / 5.9 / 5.9 (7) |
| sel-cold-A2 | a36f193d | `20261125-141744-d83a97` | 14:17:41 | success (exit 0) | 81.2 | 258 | 59 | – | – | 4.1 / 5.7 / 5.7 (7) |
| sel-cold-B2 | 740e905d | `20261125-142009-823f28` | 14:20:06 | success (exit 0) | 78.5 | 257 | 54 | 16 | 675 | 4.7 / 6.8 / 6.8 (7) |
| sel-cold-A3 | a36f193d | `20261125-142240-0f55bc` | 14:22:37 | success (exit 0) | 81.2 | 257 | 55 | – | – | 4.5 / 6.3 / 6.3 (7) |
| sel-cold-B3 | 740e905d | `20261125-142509-6789cb` | 14:25:07 | success (exit 0) | 84.5 | 272 | 56 | 16 | 566 | 4.0 / 5.5 / 5.5 (7) |
| sel-seed-A | a36f193d | `20261125-142737-53eb55` | 14:27:34 | success (exit 0) | 79.7 | 253 | 55 | – | – | 3.9 / 5.2 / 5.2 (7) |
| sel-seed-B | 740e905d | `20261125-142955-f49190` | 14:29:52 | success (exit 0) | 78.1 | 257 | 56 | 16 | 507 | 3.8 / 5.1 / 5.1 (7) |
| sel-warm-A1 | a36f193d | `20261125-143211-6de3f8` | 14:32:08 | success (exit 0) | 103.8 | 255 | 56 | – | – | 2.6 / 3.8 / 3.8 (9) |
| sel-warm-B1 | 740e905d | `20261125-143438-ceeaf1` | 14:34:35 | success (exit 0) | 93.9 | 255 | 56 | 16 | 436 | 3.5 / 4.3 / 4.3 (8) |
| sel-warm-A2 | a36f193d | `20261125-143705-dad5bc` | 14:37:02 | success (exit 0) | 101.9 | 244 | 56 | – | – | 2.2 / 3.5 / 3.5 (8) |
| sel-warm-B2 | 740e905d | `20261125-143924-9b77df` | 14:39:22 | success (exit 0) | 92.6 | 246 | 53 | 13 | 391 | 3.0 / 3.9 / 3.9 (8) |

## Summary: medians per series, A vs B

| series | side | runs | duration s median [min–max] | load1 mean median | load1 p90 median | load1 peak median | peakGrantedCpus median |
|---|---|---:|---|---:|---:|---:|---:|
| warmup | A | 1 | 1416.0 [1416.0–1416.0] | 17.5 | 26.6 | 31.9 | – |
| warmup | B | 1 | 1305.8 [1305.8–1305.8] | 17.6 | 25.3 | 29.0 | 16 |
| cold | A | 1 | 735.7 [735.7–735.7] | 15.4 | 22.1 | 26.0 | – |
| cold | B | 1 | 701.7 [701.7–701.7] | 15.7 | 23.1 | 27.0 | 16 |
| sel-cold | A | 3 | 81.2 [81.2–88.7] | 4.5 | 6.3 | 6.3 | – |
| sel-cold | B | 3 | 84.5 [78.5–85.3] | 4.4 | 5.9 | 5.9 | 16 |
| sel-seed | A | 1 | 79.7 [79.7–79.7] | 3.9 | 5.2 | 5.2 | – |
| sel-seed | B | 1 | 78.1 [78.1–78.1] | 3.8 | 5.1 | 5.1 | 16 |
| sel-warm | A | 2 | 102.8 [101.9–103.8] | 2.4 | 3.6 | 3.6 | – |
| sel-warm | B | 2 | 93.2 [92.6–93.9] | 3.3 | 4.1 | 4.1 | 14.5 |

- **Full gate, empty history** (one run each, load gate 1.0): B 701.7 s
  against A 735.7 s (−4.6 %); load1 mean / p90 / peak 15.7 / 23.1 / 27.0
  against 15.4 / 22.1 / 26.0. The load is unchanged: about 4 times the CPUs
  on average, 6.5 to 7 times at its peak.
- **Selection, empty history** (3 each): B 84.5 s [78.5–85.3] against A
  81.2 s [81.2–88.7], within the spread; load1 mean 4.4 against 4.5.
- **Selection, warm history** (2 each): B 93.2 s [92.6–93.9] against A
  102.8 s [101.9–103.8] (−9.3 %), the only separation beyond the spread;
  load1 mean 3.3 against 2.4, p90 4.1 against 3.6.
- Task CPU is unchanged between A and B in every series (selection: 244–292 s;
  full gate: 2,604 s against 2,755 s).

## Anomalies and caveats

- **Warm history slows the selection on both commits**: A 102.8 s and B
  93.2 s warm, against 81.2 s and 84.5 s from an empty history, and lowers
  the load. A learned history admits fewer tasks at once than the first-cold
  rule (charged one CPU, granted all four). #1490 recovers part of that loss
  but does not close it.
- **`peakGrantedCpus` is 16 on 4 CPUs** in every B run from an empty history:
  135 of 141 samples of `cold-B1` and 14 of 18 of `sel-cold-B1` are above 4,
  median 16. That is the first-cold rule (ADR 0100 decision 2: a task with no
  history is granted the whole capacity and reserves one CPU), so four cold
  tasks grant 16. With a warm history the sum stays at or below 4 except
  `sel-warm-B2`, whose peak is 6 in one sample (the critical-path lift is the
  probable cause; not verified).
- The selection runs last 80 to 105 s, not the 3 to 5 min aimed for, and
  their load averages 2 to 5: they do not saturate 4 CPUs throughout. With
  only 7 to 9 samples of a one-minute average, their load figures are coarse.
- The selection runs started at load1 1.85 to 1.95 (gate 2.0), so the first
  samples still carry the previous run's tail.
- The container's cgroup reached its 14.3 GB memory limit at some point
  (`environment.cgroupMemory.closing.lifetimePeak` in every `session.json`).
  That peak covers the cgroup's whole lifetime, so it is not attributable to
  any one run.
- One run per commit for the full gate: no median, and no spread.
- No run failed. The CLI printed `session reporting incomplete:
  reporter_not_configured` at the end of each run (no cloud login, as
  required); it does not affect the local report or session.
