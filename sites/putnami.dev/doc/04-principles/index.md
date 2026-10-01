# Principles

Putnami is built around a small set of structural forces.
These principles define what the system guarantees and what it will refuse to compromise.

They are constraints, not aspirations.

Coverage increases over time, but violations are treated as bugs.

---

## 1. The system must be observable by construction

**Definition**
A system that cannot explain its behavior is not operable. Putnami makes runtime behavior visible by default, without relying on ad hoc instrumentation.

**Problems addressed**
- Production systems become black boxes until failure
- Debugging depends on tribal knowledge and retrofitted logs
- Observability differs between local, preview, and production
- Engineers cannot reliably answer what changed or why behavior differs

**Consequences**
- Runtime behavior emits structured signals by default
- Local, preview, and production environments expose the same signal shape
- Observability is part of the system contract, not an optional add-on
- Debugging starts from system state, not guesswork

**Tradeoffs**
- You cannot ship core execution paths without emitting signals
- You cannot partially instrument the system and call it observable
- Baseline overhead exists even for simple workloads

---

## 2. Performance is a constraint, not an optimization

**Definition**
Putnami systems are designed so that performance characteristics are visible, bounded, and enforced, not deferred or tuned reactively.

**Problems addressed**
- Prototypes degrade silently as traffic grows
- Performance issues are discovered only in production
- Abstractions hide cost until it is too late to fix
- Performance debt accumulates under the promise of later fixes

**Consequences**
- Defaults are designed to survive production load
- Performance regressions are detectable during development and CI
- Abstractions must expose their cost
- Capacity and latency expectations are explicit

**Tradeoffs**
- You cannot defer performance work to “later”
- Slow code is rejected early, even during prototyping
- Convenience abstractions that hide cost are not acceptable

---

## 3. System behavior must be deterministic and reviewable

**Definition**
Given the same inputs, the system must behave the same way, and changes to behavior must be reviewable before execution.

**Problems addressed**
- Environments drift and silently diverge
- Builds and deployments cannot be reproduced
- Rollbacks are unsafe or incomplete
- Runtime changes bypass review and accountability
- Canary and A/B experiments rely on ad hoc switches

**Consequences**
- Git defines system intent and behavior
- Deployments are pure functions of versioned state
- Multiple behaviors may coexist only if explicitly declared
- Traffic splitting for canary or A/B testing is reviewable and reversible
- Each request is attributable to a specific version or variant
- Drift is detectable and correctable

**Tradeoffs**
- You cannot change production behavior outside version control
- Experiments require upfront modeling and explicit declaration
- Quick, undocumented runtime tweaks are not allowed

---

## 4. Security is foundational, not a layer

**Definition**
Security boundaries are enforced at the system level, not retrofitted through conventions or scattered checks.

**Problems addressed**
- Security is bolted on and inconsistently applied
- Authorization logic is fragmented across the codebase
- Secrets leak through logs, configs, or tooling
- Unsafe defaults reach production

**Consequences**
- Secure defaults are enforced by the system
- Security boundaries are declared at interfaces, not buried in logic
- Unsafe paths are explicit and intentional
- The system fails closed rather than open

**Tradeoffs**
- You cannot bypass security guarantees to “move fast”
- Unsafe behavior must be explicitly acknowledged, not implicit
- Some patterns are unavailable until they can be secured structurally

---

## 5. Data ownership is non-negotiable

**Definition**
Application data belongs to the system owner. Putnami does not require opaque data stores or proprietary control planes to function.

**Problems addressed**
- Data is locked behind proprietary APIs
- Exiting platforms requires large migrations
- Compliance and residency requirements conflict with vendor constraints
- Debugging requires external support or limited access

**Consequences**
- The primary source of truth lives in data stores you control
- Schemas and migrations are versioned and portable
- Data can be exported, inspected, and backed up without vendor mediation
- The platform layer is optional and does not own application data

**Tradeoffs**
- You are responsible for operating and exiting your primary data store
- You cannot delegate data ownership to the framework or platform
- Some managed conveniences are intentionally unavailable

---

## 6. Automation is a first-class user

**Definition**
Putnami is designed to be operated safely by both humans and machines.

**Problems addressed**
- Tooling resists scripting and automation
- CLIs are designed only for interactive use
- Automation bypasses safeguards
- AI agents cannot operate predictably or safely

**Consequences**
- Interfaces are CLI-first and scriptable
- Output is deterministic and machine-readable
- Automated actions follow the same rules as human actions
- Guardrails apply uniformly across execution contexts

**Tradeoffs**
- You cannot rely on ambiguous or human-only interfaces
- Output designed for machines may feel verbose to humans
- Predictability is favored over cleverness
