// Toolchain-resolve protocol v1. The reasoning is recorded in
// doc/adr/0003-extensions-answer-toolchain-pins.md.
//
// The core asks an extension for the toolchain pins of the lock identities the
// extension's manifest lists in runtime.resolves, and the extension answers
// with the lock's next state for each one. The extension knows where a release
// of its toolchain lives, how its archives are named and which digests the
// vendor publishes. Its answer is a lock entry, from which a reader downloads,
// verifies and unpacks each archive without knowing the toolchain.
//
// The request carries no paths. The core runs the executable with the
// workspace root as its working directory and the probe's timeout, and the
// extension reads the declarations it needs from there.

package workspace

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// ToolchainResolveProtocolVersion is the wire version of
// ToolchainResolveRequest and ToolchainResolveResult. Like the probe's, it
// travels in the payload, and a mismatch is rejected rather than guessed at.
const ToolchainResolveProtocolVersion = 1

// Bounds of the toolchain-resolve exchange.
const (
	maxToolchainResolveIdentities = 64
	maxToolchainProblemBytes      = 1024
)

// ToolchainResolveRequest is the core's question to one extension: the pins of
// the lock identities that extension answers for.
type ToolchainResolveRequest struct {
	// Version is ToolchainResolveProtocolVersion.
	Version int `json:"version"`
	// Extension is the asked extension's name, echoed back in the result so a
	// misrouted answer is detectable.
	Extension string `json:"extension"`
	// Identities are the lock identities asked: the extension's
	// runtime.resolves. Between 1 and 64 valid identities, each named once;
	// the wire form is sorted.
	Identities []string `json:"identities"`
	// Locked are the workspace lock's committed entries for asked identities,
	// as committed: a lock version 3, 4 or 5 entry. They are the facts the
	// answer starts from and are not validated again here. An asked identity
	// the lock does not pin has no entry.
	Locked map[string]LockToolchainEntry `json:"locked,omitempty"`
	// Platform is the host platform, "os/arch" (lockPlatformPattern). Every
	// pin the answer records must serve it.
	Platform string `json:"platform"`
	// Offline asks for an answer made without the network: from the committed
	// pins, the workspace's declarations and the extension's own knowledge.
	Offline bool `json:"offline,omitempty"`
}

// ToolchainResolveResult is one extension's answer: the lock's next state for
// each asked identity.
//
// A pin is what the lock records for that identity. A problem fails the
// install and is shown with its remedy. An asked identity the answer omits has
// no pin: the workspace declares no release of it, and the lock keeps none.
type ToolchainResolveResult struct {
	// Version is ToolchainResolveProtocolVersion.
	Version int `json:"version"`
	// Extension is the answering extension's name.
	Extension string `json:"extension"`
	// Toolchains maps an asked identity to its resolution.
	Toolchains map[string]ToolchainResolution `json:"toolchains,omitempty"`
}

// ToolchainResolution is the answer for one identity: exactly one of Pin and
// Problem.
type ToolchainResolution struct {
	// Pin is a lock version 5 toolchain entry, which the lock records as
	// written. It covers the request's platform.
	Pin *LockToolchainEntry `json:"pin,omitempty"`
	// Problem is why the extension cannot pin the identity.
	Problem *ToolchainProblem `json:"problem,omitempty"`
}

// ToolchainProblem is an identity the extension cannot pin, such as a
// declared release the vendor does not publish for the host platform, or an
// offline request with no committed pin to keep.
type ToolchainProblem struct {
	// Message says what is wrong. Required, at most 1024 bytes.
	Message string `json:"message"`
	// Remedy says what to change, such as the declaration to write. At most
	// 1024 bytes.
	Remedy string `json:"remedy,omitempty"`
}

// normalizeToolchainResolveRequest sorts the asked identities and drops
// repeats, in a fresh slice so a caller's slice keeps its order. Authoring
// order carries no meaning, and two spellings of one question must be one
// document. ValidateToolchainResolveRequest still reports a repeat on the
// wire, because a core that sends one did not normalize.
func normalizeToolchainResolveRequest(r *ToolchainResolveRequest) {
	if r == nil || len(r.Identities) == 0 {
		return
	}
	identities := slices.Clone(r.Identities)
	sort.Strings(identities)
	r.Identities = slices.Compact(identities)
}

// cloneToolchainResolveRequest returns a deep copy, so an answer function that
// edits its request cannot change the request its answer is checked against.
func cloneToolchainResolveRequest(r ToolchainResolveRequest) ToolchainResolveRequest {
	r.Identities = slices.Clone(r.Identities)
	r.Locked = cloneToolchainEntries(r.Locked)
	return r
}

// decodeToolchainResolveStrict strict-decodes exactly one document of the
// exchange into out. An unknown member is an error, because a reader that
// skips a member answers a question it did not read, and so is a second
// document.
func decodeToolchainResolveStrict(data []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON documents")
		}
		return fmt.Errorf("trailing data: %w", err)
	}
	return nil
}

// ParseToolchainResolveRequest strict-decodes a toolchain-resolve request.
func ParseToolchainResolveRequest(data []byte) (*ToolchainResolveRequest, []diag.Diagnostic) {
	var r ToolchainResolveRequest
	if err := decodeToolchainResolveStrict(data, &r); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to parse toolchain-resolve request: %v", err),
		}
	}
	return &r, nil
}

// ValidateToolchainResolveRequest checks the request's structural invariants.
// Diagnostics follow member order, identities in wire order and locked
// entries in identity order, so one request produces one report.
func ValidateToolchainResolveRequest(r *ToolchainResolveRequest) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{diag.Errorf("nil-request", "", "toolchain-resolve request is nil")}
	}
	var diags []diag.Diagnostic
	if r.Version != ToolchainResolveProtocolVersion {
		diags = append(diags, diag.Errorf("invalid-toolchain-resolve-version", "version",
			"toolchain-resolve request version %d is not supported (want %d)", r.Version, ToolchainResolveProtocolVersion))
	}
	if strings.TrimSpace(r.Extension) == "" {
		diags = append(diags, diag.Errorf("required-field", "extension",
			"toolchain-resolve request must name the asked extension"))
	}
	switch {
	case len(r.Identities) == 0:
		diags = append(diags, diag.Errorf("required-field", "identities",
			"toolchain-resolve request must ask for at least one identity"))
	case len(r.Identities) > maxToolchainResolveIdentities:
		diags = append(diags, diag.Errorf("limit-exceeded", "identities",
			"toolchain-resolve request asks for %d identities; at most %d are allowed",
			len(r.Identities), maxToolchainResolveIdentities))
	}
	asked := make(map[string]bool, len(r.Identities))
	for i, identity := range r.Identities {
		item := fmt.Sprintf("identities[%d]", i)
		if asked[identity] {
			diags = append(diags, diag.Errorf("duplicate-value", item,
				"identity %q is asked more than once", identity))
		}
		asked[identity] = true
		if !validLockToolchainIdentity(identity) {
			diags = append(diags, diag.Errorf("invalid-value", item,
				"identity %q must match %s and be at most %d bytes",
				identity, lockToolchainIdentityPattern, lockToolchainIdentityMaxBytes))
		}
	}
	for _, identity := range sortedLockKeys(r.Locked) {
		if !asked[identity] {
			diags = append(diags, diag.Errorf("unknown-reference", "locked."+identity,
				"locked entry %q names an identity the request does not ask for", identity))
		}
	}
	switch {
	case r.Platform == "":
		diags = append(diags, diag.Errorf("required-field", "platform",
			"toolchain-resolve request must name the host platform"))
	case !validLockPlatform(r.Platform):
		diags = append(diags, diag.Errorf("invalid-value", "platform",
			"platform %q must match %s", r.Platform, lockPlatformPattern))
	}
	return diags
}

// ParseAndValidateToolchainResolveRequest combines strict parsing and
// validation.
func ParseAndValidateToolchainResolveRequest(data []byte) (*ToolchainResolveRequest, []diag.Diagnostic) {
	r, diags := ParseToolchainResolveRequest(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return r, append(diags, ValidateToolchainResolveRequest(r)...)
}

// ParseToolchainResolveResult strict-decodes a toolchain-resolve result.
func ParseToolchainResolveResult(data []byte) (*ToolchainResolveResult, []diag.Diagnostic) {
	var r ToolchainResolveResult
	if err := decodeToolchainResolveStrict(data, &r); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to parse toolchain-resolve result: %v", err),
		}
	}
	return &r, nil
}

// ValidateToolchainResolveResult checks a result on its own: the version, the
// attribution, each key's grammar, exactly one of pin and problem per
// identity, each pin against the lock version 5 entry rules and each problem
// against its bounds. ValidateToolchainResolveAnswer checks the result against
// the request it answers; a consumer runs both. Diagnostics come out in
// identity order.
func ValidateToolchainResolveResult(r *ToolchainResolveResult) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{diag.Errorf("nil-result", "", "toolchain-resolve result is nil")}
	}
	var diags []diag.Diagnostic
	if r.Version != ToolchainResolveProtocolVersion {
		diags = append(diags, diag.Errorf("invalid-toolchain-resolve-version", "version",
			"toolchain-resolve result version %d is not supported (want %d)", r.Version, ToolchainResolveProtocolVersion))
	}
	if strings.TrimSpace(r.Extension) == "" {
		diags = append(diags, diag.Errorf("required-field", "extension",
			"toolchain-resolve result must name the answering extension"))
	}
	for _, identity := range sortedLockKeys(r.Toolchains) {
		field := "toolchains." + identity
		if !validLockToolchainIdentity(identity) {
			diags = append(diags, diag.Errorf("invalid-value", field,
				"identity %q must match %s and be at most %d bytes",
				identity, lockToolchainIdentityPattern, lockToolchainIdentityMaxBytes))
		}
		resolution := r.Toolchains[identity]
		switch {
		case resolution.Pin == nil && resolution.Problem == nil:
			diags = append(diags, diag.Errorf("required-field", field,
				"toolchain %q resolution must carry a pin or a problem; an identity with no pin is omitted", identity))
		case resolution.Pin != nil && resolution.Problem != nil:
			diags = append(diags, diag.Errorf("conflicting-field", field,
				"toolchain %q resolution carries both a pin and a problem; exactly one is allowed", identity))
		}
		if resolution.Pin != nil {
			diags = append(diags, validateLockToolchainPin(field+".pin", identity, *resolution.Pin)...)
		}
		if resolution.Problem != nil {
			diags = append(diags, validateToolchainProblem(field+".problem", identity, *resolution.Problem)...)
		}
	}
	return diags
}

// ParseAndValidateToolchainResolveResult combines strict parsing and
// standalone validation.
func ParseAndValidateToolchainResolveResult(data []byte) (*ToolchainResolveResult, []diag.Diagnostic) {
	r, diags := ParseToolchainResolveResult(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return r, append(diags, ValidateToolchainResolveResult(r)...)
}

// ValidateToolchainResolveAnswer checks result as the answer to request: it is
// attributed to the asked extension, it resolves only asked identities, and
// every pin covers the request's platform with both an integrity and an
// archive. A pin that cannot serve the host is not a pin the host can install,
// so the extension answers a problem for that identity instead.
//
// It checks only the relation between the two documents and expects each to
// have passed its own validation. Diagnostics come out in identity order.
func ValidateToolchainResolveAnswer(request *ToolchainResolveRequest, result *ToolchainResolveResult) []diag.Diagnostic {
	if request == nil {
		return []diag.Diagnostic{diag.Errorf("nil-request", "", "toolchain-resolve request is nil")}
	}
	if result == nil {
		return []diag.Diagnostic{diag.Errorf("nil-result", "", "toolchain-resolve result is nil")}
	}
	var diags []diag.Diagnostic
	if result.Extension != request.Extension {
		diags = append(diags, diag.Errorf("invalid-value", "extension",
			"answer is attributed to extension %q; the request asked %q", result.Extension, request.Extension))
	}
	asked := make(map[string]bool, len(request.Identities))
	for _, identity := range request.Identities {
		asked[identity] = true
	}
	for _, identity := range sortedLockKeys(result.Toolchains) {
		field := "toolchains." + identity
		if !asked[identity] {
			diags = append(diags, diag.Errorf("unknown-reference", field,
				"answer resolves toolchain %q, which the request does not ask for", identity))
			continue
		}
		pin := result.Toolchains[identity].Pin
		if pin == nil {
			continue
		}
		if _, ok := pin.Integrities[request.Platform]; !ok {
			diags = append(diags, diag.Errorf("required-field", field+".pin.integrities."+request.Platform,
				"toolchain %q pin records no integrity for the host platform %s; a pin that cannot serve the host is answered as a problem",
				identity, request.Platform))
		}
		if _, ok := pin.Archives[request.Platform]; !ok {
			diags = append(diags, diag.Errorf("required-field", field+".pin.archives."+request.Platform,
				"toolchain %q pin records no archive for the host platform %s; a pin that cannot serve the host is answered as a problem",
				identity, request.Platform))
		}
	}
	return diags
}

// validateToolchainProblem checks one problem's message and remedy.
func validateToolchainProblem(field, identity string, problem ToolchainProblem) []diag.Diagnostic {
	var diags []diag.Diagnostic
	switch {
	case strings.TrimSpace(problem.Message) == "":
		diags = append(diags, diag.Errorf("required-field", field+".message",
			"toolchain %q problem must say what is wrong", identity))
	case len(problem.Message) > maxToolchainProblemBytes:
		diags = append(diags, diag.Errorf("limit-exceeded", field+".message",
			"toolchain %q problem message is %d bytes; at most %d are allowed",
			identity, len(problem.Message), maxToolchainProblemBytes))
	}
	if len(problem.Remedy) > maxToolchainProblemBytes {
		diags = append(diags, diag.Errorf("limit-exceeded", field+".remedy",
			"toolchain %q problem remedy is %d bytes; at most %d are allowed",
			identity, len(problem.Remedy), maxToolchainProblemBytes))
	}
	return diags
}
