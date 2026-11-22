# ADR 0004 — Test cases are an optional member of version 1

- **Status**: Accepted
- **Date**: 2026-11-22
- **Amends**: [ADR 0001](0001-one-self-reported-change-evidence-document.md),
  the tests member of each component

## Context

A tests entry carries the passed, failed and skipped counts of a component
and up to 50 of its failing cases. A reader that compares two changes cannot
tell a test that disappeared from one that still passes: a test deleted
together with a new one leaves every count as it was. A reader that shows
what a change would keep of the tests it observed needs each case that ran.

Two shapes carry the cases. A count per outcome with a digest of the names
is small, but a reader learns that something changed, not which test. A list
of the cases, each with a stable id and its outcome, names the test; its size
grows with the suite, so it needs bounds a reader can rely on.

## Decision

1. A tests entry takes two optional members: `cases`, one entry per test
   case that ran in the component, with its `suite`, its `name` and its
   `outcome` (`passed`, `failed` or `skipped`, a closed vocabulary), and
   `omittedCases`, the number of cases the producer left out to keep within
   the bounds, or because their report gave their id to another case.
2. A case's suite and name are its stable id, unique in its entry. The
   producer that reads one test twice under one id lists it once, failed
   when any run failed, else passed when any run passed. The listed cases of
   an outcome never outnumber that outcome's count, and the listed and
   omitted cases together never outnumber the entry's cases.
3. A tests entry lists at most 10000 cases and a document at most 50000 over
   all its entries. A producer that reaches a bound lists fewer cases,
   counts the rest in `omittedCases` and says so in a diagnostic; the counts
   still cover every case.
4. The members belong to version 1. A document without them validates as
   before; a reader built before them refuses a document that carries them
   (`evidence.unknown_field`), so a reader updates before its producers write
   them, and an uploader sends them only to a reader that accepts them.

## Consequences

- A reader compares two documents case by case, and concludes that a case
  vanished only when the later entry carries `cases` and its `omittedCases`
  is 0 (`ComponentTests.ListsEveryCase`). An entry without `cases`, as every
  producer before this decision writes, says nothing of its cases.
- A report that gives two tests one suite and name, such as two top-level
  node:test tests of two files, or that holds a rerun, lists one case and
  counts the others in `omittedCases`: the entry no longer lists every case,
  and the producer says so in a diagnostic.
- A test renamed in the change reads as one case gone and one added: the id
  is what the test report states, not a judgement of the producer.
- Adding an outcome is a protocol change; reading another test report format
  is a producer change.
- The failing cases keep their messages and files; the cases carry neither,
  to stay small.
