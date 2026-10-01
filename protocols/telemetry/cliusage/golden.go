package cliusage

import _ "embed"

// GoldenLogsJSON is the canonical OTLP/JSON logs envelope a Putnami CLI usage
// session produces — one session:end failure record with the full envelope. It
// is the single importable copy of the wire shape: the CLI's encoder golden-tests
// its output against these exact bytes (minus trailing whitespace), and the
// package's own conformance test proves it satisfies ValidateLogs. Any consumer
// that needs a reference payload imports this instead of vendoring a copy.
//
//go:embed otlp-logs.golden.json
var GoldenLogsJSON []byte
