# Gate production builds with the doctor preflight

You will make `putnami build --profile production` refuse to build when a project is not production-ready — a missing required secret, a volatile datastore, an ephemeral signing key — while your everyday local builds stay exactly as fast and quiet as before. The check is the same `putnami doctor` engine, now wired in front of the build so an unsafe configuration cannot ship by accident.

The idea: a deployment *profile* decides how strict the toolchain is. Locally you build under `dev` and nothing gates you. In CI you can preview problems under `test`. When you build for production you build under `production`, and the doctor runs as a **fail-closed preflight**: any unwaived high or critical finding aborts the build before a single job runs.

## What each profile guarantees

The profile is resolved once per invocation, in this order: the `--profile` flag, then `PUTNAMI_PROFILE`, then the workspace config, then `dev` as the default.

| profile | doctor gate | severity of an unsafe state | net effect |
|---|---|---|---|
| `dev` (default) | **none** — the engine is never invoked | info | zero-config local builds are never gated |
| `test` | report-only | warning | findings are advisory; the build still runs |
| `production` | **fail-closed** | high, or critical for a secret / data-loss / auth-loss state | an unwaived high/critical finding aborts the build with exit 2 |

Two guarantees are load-bearing:

- **`dev` and `test` never block a build.** Under `dev` the preflight is a complete no-op: the doctor engine is not even run, so a fresh checkout builds with no config and no ceremony. Under `test` the same findings surface for visibility but never fail the build.
- **`production` fails closed.** If the gate cannot prove the tree is safe — an unwaived high/critical finding, or a `doctor.waivers.json` that is malformed — it aborts. A broken waiver file never silently weakens the gate.

## Steps

### 1) Build for production

Run your production build with the profile set:

```bash
putnami build --profile production
```

The preflight runs the doctor over the **selected** projects, between planning and execution. If everything is clean (or every blocking finding is waived), the build proceeds normally. If not, you get the report and a non-zero exit:

```
  Production preflight (doctor · profile production) blocked the build:
    [critical] doctor.missing_required_config /api database.password
      required configuration "database.password" is set by no committed config source under the production profile
```

The build stops here — no jobs execute. In a machine-readable mode (`--output=json` or `--output=jsonl`) the same report travels as a structured failure envelope so an agent or a CI pipeline can consume the findings programmatically.

### 2) Fix the finding — or waive it, on purpose

Most findings are fixed by supplying the missing value or binding a durable resource. When a finding is a conscious, reviewed exception (for example, a value your platform injects at deploy time), record it in the committed workspace-root `doctor.waivers.json`:

```json
{
  "protocolVersion": 1,
  "waivers": [
    {
      "code": "doctor.missing_required_config",
      "project": "api",
      "field": "database.password",
      "owner": "platform-team",
      "reason": "database.password is injected by the platform at deploy time; OPS-1234",
      "expires": "2026-12-31"
    }
  ]
}
```

A waiver is reviewed like any other source. Its rules are strict and fail-closed:

- A live, structurally valid waiver suppresses its matching finding — the finding stays in the report with its provenance, but it no longer blocks.
- **Scope a waiver to the field you actually reviewed.** With `field` set, the waiver suppresses only the finding for that config path. Omit `field` and the waiver suppresses *every* finding of that code in the project — so a broad `doctor.missing_required_config` waiver would also silently accept a missing secret you never meant to, while a `field`-scoped one leaves the others blocking. You can list several `field`-scoped waivers for the same code and project, one per accepted finding.
- An **expired** waiver never suppresses anything and surfaces a `doctor.waiver_expired` finding of its own; the underlying problem still blocks.
- A waiver naming an unknown check code becomes a `doctor.waiver_unknown_code` finding and matches nothing.
- Any other structural defect (a missing `owner`/`reason`, a bad `expires`, a duplicate) makes the whole file untrustworthy: **no** waiver is applied and the build fails closed.

### 3) Preview findings without building

To see what production would flag without running a build, invoke the doctor directly:

```bash
putnami doctor --profile production
```

This is the read-only form of the same engine. Use it in review, or run it under `test` in CI to keep findings visible without failing the pipeline.

## A note on `--trace-profile`

`--profile` selects the *deployment* profile (`dev` | `test` | `production`) described here. It is unrelated to performance profiling: the Chrome-trace output flag is `--trace-profile <path>`. If you pass a file path to `--profile` you will get a usage error pointing you at `--trace-profile`.

## Deterministic and reviewable

The preflight report is a pure function of the committed tree, the profile, and a single injected clock (used only to evaluate waiver expiry). It reads only committed artifacts, never runtime state or the network, and never a resolved config value — evidence names *where* to look, never *what* was found. The same tree always produces the same report, so a gate result is reproducible in review and in CI.

You now have a production build that cannot ship an unsafe configuration by accident, a reviewed, expiring escape hatch for the exceptions you accept on purpose, and local builds that stay completely ungated.
