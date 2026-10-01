# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in Putnami, please report it responsibly. **Do not open a public GitHub issue for security vulnerabilities.**

### How to Report

Email **security@putnami.com** with:

- A description of the vulnerability
- Steps to reproduce the issue
- The potential impact
- Any suggested fix (if you have one)

### What to Expect

- **Acknowledgment** within 3 business days of your report
- **Status update** within 10 business days with an initial assessment
- **Resolution timeline** communicated once the issue is triaged

We will work with you to understand and address the issue before any public disclosure.

## The first 72 hours

The two windows above are what we commit to you. This section is the triage that
happens inside them, so you know what is being done before the status update
arrives.

The clock is **72 wall-clock hours from the first read of your report**, and it
runs inside the 3-business-day acknowledgment window rather than beside it. When
a weekend or a holiday makes the two disagree, the business-day windows above are
the commitment; this one is the working target.

| Hours | What happens |
|---|---|
| 0 – 24 | We reproduce the report, or write down precisely why we cannot. We decide whether it is in [scope](#scope) and assign a severity: critical, high, moderate, or low. |
| 24 – 48 | We identify the affected versions and platforms against the [First Public-Release Contract](README.md#first-public-release-contract), decide whether the fix is developed privately, and name one owner for it. |
| 48 – 72 | A fix or a mitigation exists at least in draft, or the report is escalated with that named owner and a date. Either way you get the triage outcome and the severity we assigned. |

Two properties matter more than the hours. Severity is assigned from impact on
the supported core, not from how the report was written. And a report we cannot
reproduce is answered with what we tried, not closed in silence.

## Patch policy

| Severity | How the fix ships |
|---|---|
| Critical or high | A patch release off the released tag, targeted within 30 calendar days of acknowledgment. If we are going to miss that, we say so before the date, with a new one. |
| Moderate or low | The next minor release, unless the fix is safe enough to travel as a patch. |

A security patch carries the fix and its test and nothing else, and it never
moves an artifact format's supported version window — those are
[release rules that apply to every patch](RELEASING.md#what-a-release-may-contain).

No security fix ships silently. Once disclosure is agreed, the release notes name
the issue, the affected versions, and the fixed version.

## Supported Versions

Security updates are applied to the latest release. We recommend always running the most recent version of Putnami.

The supported product surface and platform matrix are defined by the
[First Public-Release Contract](README.md#first-public-release-contract).
Who approves a security release, and who can roll one back, is recorded in
[GOVERNANCE.md](GOVERNANCE.md#how-decisions-are-made).

## Disclosure Policy

- We follow coordinated disclosure. Please allow us reasonable time to address the issue before public disclosure.
- We will credit reporters in the release notes (unless you prefer to remain anonymous).
- We will not take legal action against researchers who report vulnerabilities responsibly and in good faith.

## Scope

This policy covers the Putnami framework and its official packages published under the `@putnami` scope. It does not cover third-party extensions or applications built with Putnami.
Accepting vulnerability reports for an experimental public package does not
change that package's reviewed support status.

## Security Design

Putnami treats security as foundational:

- Secure defaults are enforced
- The system fails closed
- Unsafe paths require explicit acknowledgment

If you believe any default behavior is insecure, that is considered a bug and should be reported.
