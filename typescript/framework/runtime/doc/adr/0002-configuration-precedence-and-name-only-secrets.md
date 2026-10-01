# ADR 0002 — Fix configuration precedence and keep secrets name-only

- **Status**: accepted
- **Scope**: `@putnami/runtime` (`typescript/framework/runtime`)

## Context

A workload reads configuration from committed files, a build-merged dependency
file, local secrets, extension sources, inline `CONFIG_DATA`, and environment
variables. "Which one won?" must have the same answer everywhere. Configuration
is the most quoted value in an application (startup logs, validation errors,
debug dumps, infrastructure manifests), so redaction cannot depend on each call
site remembering. The browser has none of these sources.

## Decision

1. **One precedence list**, merged lowest first: `conf/.env.yaml`,
   `conf/.env.<env>.yaml`, `.gen/conf/.env.<env>.yaml` (build-merged from
   dependencies), `conf/.secrets.<env>.yaml`, extension sources at their declared
   priority, then `CONFIG_DATA`. A field-level `Env()` fills only keys still
   absent afterwards, so any file or `CONFIG_DATA` value beats an ambient
   variable. `${VAR}` and `${VAR:-default}` references are interpolated after
   the merge; an unresolved one without a default stays in place so validation
   reports it.
2. **Reads fail loudly.** A missing required field or a type mismatch raises;
   no partial object is returned. `CONFIG_SERVER_URL` with no registered source
   serving it fails startup instead of falling back to local sources.
3. **Sensitive fields are name-only.** A `Sensitive()` field is redacted in
   validation errors and in the config description, and contributes only its
   name to the infrastructure requirements. Secret names derive from the same
   declaration, so the manifest cannot miss one.
4. **Parsed configuration is untrusted.** `__proto__`, `constructor`, and
   `prototype` keys are stripped before merging.
5. **One owner per path.** A dependency contributes its block under its own path.
   Registering the identical definition twice is a no-op; two different
   definitions for one path fail.
6. **The browser entry is a separate surface.** It applies declared defaults and
   call-site overrides only. It does not validate, throw on missing fields, or
   read files, `CONFIG_DATA`, or environment variables.

## Rejected alternatives

- **Environment variables beat files.** An ambient variable silently overrides
  the committed value, invisibly to review.
- **Partial configuration checked by each reader.** The reader that forgets
  fails far from the cause.
- **Redact at the log sink.** The sink sees a value, not a declaration, and each
  new sink reopens the hole.
- **Secret values in infrastructure requirements.** The manifest is committed;
  it states which secrets exist, never their values.
- **One entry for server and browser.** The bundle pulls the server loader, and
  required-field validation fails in the browser.

## Consequences

- A new configuration source needs a stated position in the precedence list.
- Nobody can print a sensitive field, including its owner; diagnose a wrong
  secret by its source, not its value.
- Browser and server `useConfig` differ in failure behavior; shared code must
  not rely on validation.
- Changing the precedence order is a breaking change even when no schema
  changes.
