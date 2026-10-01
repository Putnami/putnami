# ADR 0001 — A broken constraint declaration is a fault, not a passing field

- **Status**: accepted
- **Scope**: `go.putnami.dev/schema` (`go/framework/schema`)

## Context

Constraints live in unchecked struct tags. A misspelled constraint
(`validate:"requred"`) or a pattern that does not compile silently makes a
mandatory field optional. Panicking instead turns the typo into an outage on
the path that handles untrusted input. And when validation returns both errors
and data, a caller that checks only the data can act on an invalid value.

## Decision

A malformed constraint declaration is a `FieldError` on its field, reported
before any value is read and whether or not a value was supplied. Validation
continues for the other fields, and the malformed constraint is skipped for
the value check.

`Validate` never panics on a caller-supplied type. A nil or non-struct type
returns a structured schema error; pointers are dereferenced first, so `*T`
behaves like `T`.

The data map holds only fields that passed every constraint.

A `default` tag fills an absent field before the required check, so a
defaulted field satisfies `required`. Other absent fields are skipped after
the required check, which makes partial updates work without a patch schema;
presence is expressible only through `required`.

Registering a constraint replaces a built-in of the same name process-wide, so
registration belongs in composition code, not a request path.

## Rejected alternatives

- **Skip unknown constraints.** A typo becomes an unnoticed security hole.
- **Panic on a malformed tag.** It crashes the process on untrusted input.
- **Report the fault against the user's data.** The defect is the server's
  declaration.
- **Return every supplied value and let callers filter.** The safe path would
  need extra work.
- **Constrain absent fields as zero values.** Partial updates become
  impossible.

## Consequences

- The error list is the authority; a missing key alone does not prove failure.
