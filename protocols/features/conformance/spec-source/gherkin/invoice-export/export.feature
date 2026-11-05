@billing
Feature: Invoice export
  An accountant exports a closed month in under 30 seconds.

  Each export row matches its invoice.

  # A comment is no description.
  Background:
    Given a closed month with 3 invoices

  @putnami-id:export-a-closed-month
  Rule: A closed month exports as one file.
    Only a closed month exports, so the totals cannot move.

    @smoke @putnami-check:month-exported
    Scenario: The accountant exports March
      When the accountant exports March
      Then one file holds 3 rows

    Scenario Outline: An open month does not export
      When the accountant exports <month>
      Then the export is refused

      Examples:
        | month |
        | April |
        | May   |

  Rule: Each row matches its invoice
    Scenario: A row carries the invoice total
      When the accountant exports March
      Then each row total equals its invoice total
      """
      Rule: this line is a doc string, not a rule
      """
