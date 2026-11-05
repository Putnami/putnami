Feature: Export audit
  Scenario: An export leaves an audit entry
    When the accountant exports March
    Then the audit log names the accountant
