package qualify

import (
	"fmt"
	"io"
	"strings"

	qualifyproto "go.putnami.dev/protocol/qualify"
)

// RenderVerdictText writes the human verdict: one line per phase, one line per
// request, then the final `verdict:` line a reader can grep for.
func RenderVerdictText(w io.Writer, verdict *qualifyproto.Verdict) error {
	var b strings.Builder
	for _, phase := range verdict.Phases {
		fmt.Fprintf(&b, "%s %s%s %dms\n", symbol(phase.State), phase.Name, stateSuffix(phase.State), phase.DurationMs)
		for _, d := range phase.Diagnostics {
			fmt.Fprintf(&b, "    %s: %s\n", d.Code, d.Message)
		}
	}
	for _, request := range verdict.Requests {
		status := ""
		if request.Status != 0 {
			status = fmt.Sprintf(" %d", request.Status)
		}
		reason := ""
		if request.Reason != "" {
			reason = " (" + request.Reason + ")"
		}
		fmt.Fprintf(&b, "  %s %s%s%s %dms%s\n", symbol(request.State), request.ID, status, stateSuffix(request.State), request.DurationMs, reason)
	}
	fmt.Fprintf(&b, "verdict: %s (%s, %s, %s)\n", verdict.State, verdict.Project, verdict.Target.Kind, describeBinding(verdict.Binding))
	_, err := io.WriteString(w, b.String())
	return err
}

// RenderContractText writes the human contract: its identity, its sources, and
// one line per request with the route fact it came from.
func RenderContractText(w io.Writer, contract *qualifyproto.Contract, unsupported *Unsupported) error {
	var b strings.Builder
	fmt.Fprintf(&b, "contract: %s (%s)\n", contract.Project, contract.Digest)
	for _, source := range contract.DerivedFrom {
		fmt.Fprintf(&b, "derived from: %s (%s)\n", source.Path, source.Digest)
	}
	for _, request := range contract.Requests {
		fmt.Fprintf(&b, "  %s  maxStatus %d  provenance %s\n", request.ID, request.MaxStatus, request.Provenance)
	}
	fmt.Fprintf(&b, "requests: %d\n", len(contract.Requests))
	if unsupported != nil {
		fmt.Fprintf(&b, "unsupported: %s — %s\n", unsupported.Reason, unsupported.Remedy)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func symbol(state qualifyproto.State) string {
	switch state {
	case qualifyproto.StatePassed:
		return "✓"
	case qualifyproto.StateNotRun:
		return "-"
	default:
		return "✗"
	}
}

func stateSuffix(state qualifyproto.State) string {
	if state.IsPass() {
		return ""
	}
	return " " + string(state)
}

func describeBinding(binding qualifyproto.Binding) string {
	switch binding.Kind {
	case qualifyproto.BindingArtifact:
		observed := binding.ObservedSHA
		if observed == "" {
			observed = "none"
		}
		return fmt.Sprintf("artifact expected %s observed %s", binding.ExpectedSHA, observed)
	case qualifyproto.BindingTree:
		dirty := ""
		if binding.Dirty {
			dirty = " dirty"
		}
		return fmt.Sprintf("tree %s%s", shortDigest(binding.Fingerprint), dirty)
	default:
		return binding.Kind
	}
}

// shortDigest keeps the first 12 characters of a digest or a sha: enough to
// tell two apart in a sentence, short enough to read.
func shortDigest(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}
