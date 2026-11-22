# xcrun outputs

`swift/` holds what the fake `xcrun` of `xcresult_test.go` prints for the
`.xcresult` bundle of the Swift demo (`testdata/demos/swift.txtar`):
`tests.json` for `xcresulttool get test-results tests`, `report.json` and
`archive.json` for `xccov view --report --json` and `--archive --json`. They
are written by hand, in the shape of the readers module's Xcode fixtures
(see `tooling/evidence-readers/testdata/SOURCES.md`), with the cases and line
counts of the demo's converted JUnit and Cobertura reports, so that both read
to the same tests and coverage.
