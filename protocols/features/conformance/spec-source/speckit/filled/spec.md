# Feature Specification: Invoice export

**Feature Branch**: `001-invoice-export`

**Created**: 2026-11-04

**Status**: Draft

**Input**: User description: "Accountants export a month of invoices."

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Export a month (Priority: P1)

An accountant exports the invoices of one month as a CSV file.

**Why this priority**: Month-end closing depends on it.

**Independent Test**: Export a month that holds invoices and read the file.

**Acceptance Scenarios**:

<!-- putnami:id a-month-exports-as-csv -->
1. **Given** a month with invoices, **When** the accountant exports it, **Then** the file lists each invoice
2. **Given** a month with no invoice, **When** the accountant exports it, **Then** the file holds only its header

---

### Edge Cases

- What happens when an invoice is voided after the export?

## Requirements *(mandatory)*

### Functional Requirements

<!-- putnami:id export-a-month -->
- **FR-001**: System MUST export the invoices of one calendar month as a CSV
  file with one row per invoice.
<!-- putnami:id refuse-an-open-month -->
- **FR-002**: System MUST refuse a month that has not closed.

### Key Entities *(include if feature involves data)*

- **Invoice**: A billed amount with a number, a date and a customer.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: An accountant exports a closed month in under 30 seconds.
- **SC-002**: Each export row matches its invoice.

## Out of Scope

- Exporting to a spreadsheet format other than CSV.

## Assumptions

- Accountants already have access to the billing application.
