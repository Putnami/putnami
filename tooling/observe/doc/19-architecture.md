# Architecture rules

`putnami observe` shows the architecture rules your CI already checks with
ArchUnit, deptrac or import-linter: each rule, kept or broken, and each
violation with its file, its line and the component that holds the file. It
runs no checker and reads no rule configuration: your CI runs the checker and
keeps its report, and you rewrite no rule.

## The reports it reads

| Checker | Language | Report | How `putnami observe` finds it |
|---|---|---|---|
| ArchUnit | Java, Kotlin | The JUnit report of the test class that runs the rules, as Maven Surefire or Gradle writes it | A case whose test source imports `com.tngtech.archunit`, or whose class name matches a pattern of `--archunit-classes`, is a rule rather than a test |
| deptrac | PHP | `vendor/bin/deptrac analyse --formatter=junit --output=deptrac.xml` | A JUnit report whose suites deptrac wrote: an empty `package`, the `localhost` hostname, and cases named `Layer - Class` |
| import-linter | Python | The text `lint-imports` prints, saved with `lint-imports > import-linter.txt` | A `.txt` file whose name contains `lint` and that holds the `Analyzed N files, M dependencies.` line |

`--architecture` names the deptrac and import-linter reports instead, as a
comma-separated list of paths or glob patterns. A named file that is neither
is a diagnostic. A deptrac report is never read as a test report.

## What each rule is

| Checker | A rule is | A violation is |
|---|---|---|
| ArchUnit | One `@ArchTest` rule, named by the text of its `Rule '...'` clause, else by the test case | One line of its failure, `... in (Quotes.java:8)`, counted from `was violated (N times)` |
| deptrac | One layer, named by the layer: broken when a class of it depends on a layer the ruleset forbids | One `failure`, `Depender:N must not depend on Dependent (A on B)` |
| import-linter | One contract, named by its `name` | One import chain under `Broken contracts`, `shared.money -> api.main (l.3)` |

A passing ArchUnit case is a kept rule; a skipped one is no rule. An ArchUnit
case counts in no component's tests. One rule read twice, such as one ArchUnit
rule run by two modules, is one rule: broken when either run broke it, its
violations summed.

## Where a violation goes

A violation goes to the source file of the class or module it names, and to
the component that holds that file:

| Checker | The source file is |
|---|---|
| ArchUnit | The class under `src/main/java` or `src/main/kotlin`, then under the test source roots, of the component that wrote the report first, with the file name the violation states |
| deptrac | The class through the PSR-4 `autoload` and `autoload-dev` prefixes of the `composer.json` of the repository root and of each component, the longest prefix first |
| import-linter | The module as a `.py` file or a package `__init__.py`, under the repository root, `src`, a component or its `src` |

A violation whose file is not found keeps its message, without a file or a
component.

## What it prints

When it read a rule, the summary counts the rules and lists each broken one
with its first five violations:

```text
  architecture  3 rules, 2 broken
    broken    deptrac: Controller (2 violations)
      . src/Controller/QuoteController.php:7: App\Controller\QuoteController must not depend on App\Repository\QuoteRepository (Controller on Repository)
      . src/Controller/QuoteController.php:12: App\Controller\QuoteController must not depend on App\Repository\QuoteRepository (Controller on Repository)
    broken    deptrac: Pricing (2 violations)
      packages/pricing packages/pricing/src/Calculator.php:7: Acme\Pricing\Calculator must not depend on App\Service\QuoteService (Pricing on Service)
      packages/pricing packages/pricing/src/Calculator.php:16: Acme\Pricing\Calculator must not depend on App\Service\QuoteService (Pricing on Service)
```

The evidence document carries every rule, kept or broken, in its
`architecture` member, at most 1000 rules and 50 violations per rule.

## What it does not do

- It reports the rules the job's checker found. It does not compare them with
  the base: a rule the base already broke shows as broken.
- It does not fail the job. Keep the checker's own step to fail it.
