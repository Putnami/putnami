# telemetry.putnami.dev

The receiver behind `https://telemetry.putnami.dev`: the endpoint the Putnami
CLI sends its anonymous usage events to, plus the private aggregate read the
maintainers use to answer product questions.

It is an ordinary Putnami Go workload — the same framework, the same
`putnami build`/`putnami serve` workflow, the same deploy path as any other
project in this workspace. Nothing about it is privileged.

## What a user needs to know

Everything that decides what leaves your machine, what is stored, for how long,
and how to turn it off is documented once, for users, at
[putnami.dev/docs/concepts/cli-telemetry](https://putnami.dev/docs/concepts/cli-telemetry).
That page is the canonical statement; this README does not restate it and must
not contradict it.

The short version, so nobody has to guess while reading this code:

- The CLI records a closed, allowlisted set of usage signals — never code, file
  paths, project or workspace names, environment variables, configuration,
  error messages, or user identity.
- Collection is disabled until an interactive notice has been shown on the
  machine, and CI is off by default.
- `putnami telemetry off`, `DO_NOT_TRACK=1`, or `PUTNAMI_TELEMETRY=off` disable
  it; the CLI is fully functional either way.
- Delivery is fail-silent: this service being slow, broken, or unreachable never
  changes the result of a CLI command.

## What this workload does

| Route | Method | Auth | Purpose |
|---|---|---|---|
| `/v1/logs` | `POST` | none — anonymous by design | OTLP/JSON CLI-usage ingest |
| `/v1/cli-usage/aggregate` | `GET`, `HEAD` | OIDC bearer, fail-closed | private aggregate read for internal readers |

Ingest sanitizes each record against the receiver-side allowlist before storing
anything, and drops caller IP addresses. The aggregate side keeps product counts
plus a short-lived membership projection whose only job is to suppress groups
with too few distinct contributors; it holds no raw events and is never exposed.

The declared public surface, what edge enforcement would change, and the tests
that pin each claim are in
[`doc/public-route-contract.md`](doc/public-route-contract.md). The canonical
artifact is [`schema/http-routes.json`](schema/http-routes.json), produced by
`go.putnami.dev/http` `ServerPlugin.Describe` and committed from the same
describe the build runs.

## Boundaries

- **This is not application telemetry.** Telemetry inside applications you build
  with the Go or TypeScript frameworks is separately configured, opt-in, and
  unaffected by anything here.
- **This service is not a supported surface.** `putnami.support.json` classifies
  packages and protocols — things you depend on — not services Putnami operates.
  The wire contract it speaks *is* classified: see
  [`protocols/telemetry`](../../protocols/telemetry/README.md).
- **No availability promise.** Nothing in this repository commits to an uptime
  or retention guarantee for the hosted receiver.

## Development

```bash
putnami test --projects telemetry.putnami.dev
putnami build --projects telemetry.putnami.dev
putnami serve telemetry.putnami.dev
```

Any change to the served surface must regenerate `schema/http-routes.json` and
update `doc/public-route-contract.md` in the same change; `route_contract_test.go`
fails when the committed artifact and the described routes disagree.
