package clicore

import (
	"encoding/json"
	"fmt"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
)

// OutputJSON and related constants alias the shared protocol CLI output modes
// for existing clicore callers.
const (
	OutputJSON         = protocolcli.OutputJSON
	OutputJSONL        = protocolcli.OutputJSONL
	OutputText         = protocolcli.OutputText
	OutputCloudLogging = protocolcli.OutputCloudLogging
)

// ResolveOutputMode resolves --output plus the --json alias through the shared
// protocol CLI contract.
func ResolveOutputMode(params map[string]any) (protocolcli.OutputMode, error) {
	return protocolcli.ResolveOutputMode(StringParam(params, "output"), Truthy(Param(params, "json")))
}

// ValidateOutputMode returns a usage-classified error when --output/--json do
// not satisfy the shared protocol CLI conflict policy.
func ValidateOutputMode(params map[string]any) error {
	if _, err := ResolveOutputMode(params); err != nil {
		return NewError(err.Error(), ExitUsage)
	}
	return nil
}

// StructuredOutput reports whether the caller asked for machine-readable output
// through any canonical selector: --output=json, --output=jsonl, or the --json
// alias. It is the boolean gate every command shares so "structured or human"
// has one meaning across the CLI.
func StructuredOutput(params map[string]any) bool {
	mode, err := ResolveOutputMode(params)
	return err == nil && mode.IsStructured()
}

// WriteResult emits data as structured JSON when any canonical output selector
// is set (--output=json|jsonl or the --json alias), otherwise it prints the
// human message. Structured one-shot commands render the shared protocol CLI
// version-2 result envelope; the jsonl/json distinction controls compact vs
// indented envelope rendering.
func WriteResult(data any, params map[string]any, ioctx IO, message string) {
	mode, err := ResolveOutputMode(params)
	if err != nil {
		return
	}
	if mode.IsStructured() {
		WriteProtocolResult(protocolcli.NewResultV2(CommandPath(params), data, nil), mode, ioctx)
		return
	}
	ioctx.Stdout(message)
}

// WriteErrorResult emits the shared protocol CLI failure envelope when the
// selected output mode is structured.
func WriteErrorResult(err error, params map[string]any, ioctx IO) {
	mode, resolveErr := ResolveOutputMode(params)
	if resolveErr != nil && !Truthy(Param(params, "json")) && StringParam(params, "output") == "" {
		return
	}
	if resolveErr != nil {
		mode = protocolcli.OutputJSON
	}
	if mode.IsStructured() {
		result := protocolcli.NewResultV2(CommandPath(params), protocolcli.ResultData(err), err)
		result.ExitCode = ExitCode(err)
		WriteProtocolResult(result, mode, ioctx)
	}
}

// WriteProtocolResult renders the shared protocol CLI version-2 result envelope
// through the IO JSON hook when present, or through stdout when running as the
// standalone extension binary.
func WriteProtocolResult(result protocolcli.ResultV2, mode protocolcli.OutputMode, ioctx IO) {
	if ioctx.JSON != nil {
		ioctx.JSON(result)
		return
	}
	if mode == protocolcli.OutputAuto {
		mode = protocolcli.OutputJSON
	}
	var b strings.Builder
	if _, err := protocolcli.WriteResultV2(&b, mode, result); err != nil {
		if ioctx.Stderr != nil {
			ioctx.Stderr(fmt.Sprintf("write result: %v", err))
		}
		return
	}
	WriteTextLines(ioctx, b.String())
}

// CommandPath returns the command path used in the protocol CLI result
// envelope.
func CommandPath(params map[string]any) string {
	if command := StringParam(params, "command", "commandPath"); command != "" {
		return command
	}
	return "cloud"
}

// WriteJSON sends data through the IO JSON hook, falling back to indented text.
func WriteJSON(ioctx IO, data any) {
	if ioctx.JSON != nil {
		ioctx.JSON(data)
		return
	}
	WriteJSONText(ioctx, data)
}

// WriteJSONText prints data as indented JSON over the Stdout sink.
func WriteJSONText(ioctx IO, data any) {
	encoded, _ := json.MarshalIndent(data, "", "  ")
	WriteTextLines(ioctx, string(encoded))
}

// WriteTextLines splits text on newlines and prints each over the Stdout sink.
func WriteTextLines(ioctx IO, text string) {
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if line == "" {
			continue
		}
		ioctx.Stdout(line)
	}
}
