package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"go.putnami.dev/intelligence/agent-readiness/internal/command"
	protocolcli "go.putnami.dev/protocol/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/resultv2"
	"go.putnami.dev/sdk/extension/runtimeinfo"
)

const extensionName = "@putnami/agent-readiness"

// The Go packager stamps this from the project version at release time.
var runtimeVersion string

func collectorVersion() string {
	if runtimeVersion != "" {
		return runtimeVersion
	}
	return "dev"
}

func main() { os.Exit(runMain(os.Args[1:], os.Stdout, os.Stderr, http.DefaultClient)) }

func runMain(args []string, stdout, stderr io.Writer, client *http.Client) int {
	if handled, err := runtimeinfo.Handle(args, stdout, extensionName, runtimeVersion); handled {
		if err != nil {
			fmt.Fprintln(stderr, err)
			return protocolcli.ExitFailure
		}
		return protocolcli.ExitSuccess
	}
	if len(args) == 0 || args[0] != "agent-readiness" {
		return resultv2.Emit(stdout, stderr, protocolcli.OutputAuto, command.Command, "", nil,
			protocolcli.Usagef("unknown @putnami/agent-readiness command"))
	}
	params, contextFile, err := parseArgs(args[1:])
	if err != nil {
		return resultv2.Emit(stdout, stderr, protocolcli.OutputAuto, command.Command, "", nil, err)
	}
	var jobContext *pctx.Context
	if contextFile != "" {
		jobContext, err = pctx.Parse(contextFile)
		if err != nil {
			return resultv2.Emit(stdout, stderr, protocolcli.OutputAuto, command.Command, "", nil,
				protocolcli.Usagef("read Putnami context: %v", err))
		}
		for key, value := range jobContext.Params {
			if _, overridden := params[key]; !overridden {
				params[key] = value
			}
		}
	}
	mode, err := protocolcli.ResolveOutputMode(paramString(params, "output"), paramBool(params, "json"))
	if err != nil {
		return resultv2.Emit(stdout, stderr, protocolcli.OutputAuto, command.Command, "", nil, protocolcli.Classify(err, protocolcli.ErrUsage))
	}
	options, err := command.ParseOptions(params)
	if err != nil {
		return resultv2.Emit(stdout, stderr, mode, command.Command, "", nil, err)
	}
	dir := ""
	if jobContext != nil && jobContext.UserScope != nil {
		dir = jobContext.UserScope.CallerDir
	}
	config := command.Config{Dir: dir, CollectorVersion: collectorVersion(), APIURL: apiURL(),
		Client: client, Stdout: stdout, Stderr: stderr, Mode: mode}
	result, err := command.Run(context.Background(), options, config)
	if options.PrintPayload && err == nil {
		return protocolcli.ExitSuccess
	}
	return resultv2.Emit(stdout, stderr, mode, command.Command, "", result, err)
}

func parseArgs(args []string) (map[string]json.RawMessage, string, error) {
	params := map[string]json.RawMessage{}
	contextFile := ""
	for len(args) > 0 {
		arg := args[0]
		args = args[1:]
		name, inline, hasInline := strings.Cut(arg, "=")
		switch name {
		case "--putnamiContext", "--timeout", "--output":
			value := inline
			if !hasInline {
				if len(args) == 0 {
					return nil, "", protocolcli.Usagef("%s requires a value", name)
				}
				value, args = args[0], args[1:]
			}
			if name == "--putnamiContext" {
				contextFile = value
				continue
			}
			encoded, _ := json.Marshal(value)
			params[strings.TrimPrefix(name, "--")] = encoded
		case "--json", "--print-payload":
			if hasInline {
				return nil, "", protocolcli.Usagef("%s takes no value", name)
			}
			params[strings.TrimPrefix(name, "--")] = json.RawMessage("true")
		default:
			return nil, "", protocolcli.Usagef("unknown agent-readiness argument: %s", arg)
		}
	}
	return params, contextFile, nil
}

func paramString(params map[string]json.RawMessage, name string) string {
	var value string
	_ = json.Unmarshal(params[name], &value)
	return value
}

func paramBool(params map[string]json.RawMessage, name string) bool {
	return pctx.Params(params).Bool(name, false)
}

func apiURL() string {
	for _, name := range []string{"PUTNAMI_CLOUD_API_URL", "PUTNAMI_CONTROL_PLANE_URL"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return strings.TrimRight(value, "/")
		}
	}
	return "https://api.putnami.cloud"
}
