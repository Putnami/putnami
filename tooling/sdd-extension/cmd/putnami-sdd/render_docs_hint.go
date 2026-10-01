package main

import (
	"fmt"
	"io"
)

// validationDocsURL points a failing validation at the published page that
// explains exactly which conditions fail and which only warn. The hint prints
// on failure paths only: the success outputs are parity-recorded
// (tooling/cli/internal/cli/testdata/sdd-parity) and must stay byte-stable.
const validationDocsURL = "https://putnami.dev/docs/spec-driven-development/validation-jobs"

func printDocsHint(w io.Writer) {
	fmt.Fprintf(w, "  Docs: %s\n", validationDocsURL)
}
