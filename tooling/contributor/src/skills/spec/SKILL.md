---
name: spec
description: Define an outcome and acceptance in the owning native feature specification, with proportional independent challenge
---

# Spec

Clarify what observable outcome the request needs and how a consumer will
recognize it. Use the owning project's native feature declaration and spec as
the authoritative requirement inventory. Reuse an existing current spec for a
settled correction; do not create a document to satisfy a stage count.

From the consumer workspace root, select the CLI again in each shell:

```bash
PUTNAMI_CLI=putnami
if [ -x ./putnamiw ]; then
  PUTNAMI_CLI=./putnamiw
fi
```

Read repository instructions and the accepted mandate. Discover with Putnami
first, then inspect the native feature, spec, decisions, source projects and
applicable recipe. Follow [build](../build/SKILL.md) for ownership, authority
and progress, and its [workflow contract](../../../.agents/skills/build/references/workflow.md)
for exact versioned references. Planning alone is read-only unless this
invocation authorizes editing a native spec. `spec` grants no task creation,
publication, merge or delivery authority.

State the requested outcome, in-scope and later requirements, acceptance
scenarios, explicit non-goals and unknown decisions. Keep a requested but
deferred requirement visible with its actual scope authority. Name the smallest
useful delivery unit and its first inspectable result; keep the next cycle
precise. Native requirement IDs and feature-declared verification checks are
the source of truth; do not duplicate them in a local list of expected checks.
Existing feature/spec/ADR schemas stay closed. If a requirement cannot be
checked by the native producer, record the actual acceptance evidence needed
instead of declaring it verified.

For a material new or changed proposal, request an independent
[review](../review/SKILL.md) of the exact spec and relevant context versions.
Preserve its original report, findings and resolution references. A small
settled correction can reuse current agreement when its examined inputs still
match. Return the native source path/version, acceptance behavior, open
questions, challenge result and the next result/action. A spec is intent, not
implementation or producer proof.
