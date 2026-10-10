package cloudcli

import (
	"context"
	"fmt"
	"sync"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	configcli "go.putnami.dev/cloud/extension/internal/configcli"
	datacli "go.putnami.dev/cloud/extension/internal/datacli"
	deliverycli "go.putnami.dev/cloud/extension/internal/deliverycli"
	distributioncli "go.putnami.dev/cloud/extension/internal/distributioncli"
	identitycli "go.putnami.dev/cloud/extension/internal/identitycli"
	runtimecli "go.putnami.dev/cloud/extension/internal/runtimecli"
	sourcecli "go.putnami.dev/cloud/extension/internal/sourcecli"
)

// `putnami cloud status` is the synthesis of the public root: one line per
// entry that has a state, each ok, degraded, failing or unknown, with the command that shows more. Every entry is the same node
// its own `putnami cloud <entry> status` prints, metrics included in the
// structured output. An entry that errors or does not answer in time is
// unknown, and every other entry still prints.

// statusProbeTimeout bounds each entry.
var statusProbeTimeout = 15 * time.Second

// statusEntries is the order the synthesis prints its lines in: the public
// root order, restricted to the entries that have a state.
var statusEntries = []string{
	"whoami", "token", "registries", "packages", "channels", "ci", "cache",
	"source", "config", "secrets", "db", "env",
}

// statusMore is the command that shows each entry in full.
var statusMore = map[string]string{
	"whoami":     "putnami cloud whoami",
	"token":      "putnami cloud token status",
	"registries": "putnami cloud registries status",
	"packages":   "putnami cloud packages status",
	"channels":   "putnami cloud channels status",
	"ci":         "putnami cloud ci status",
	"cache":      "putnami cloud cache status",
	"source":     "putnami cloud source status",
	"config":     "putnami cloud config status",
	"secrets":    "putnami cloud secrets status",
	"db":         "putnami cloud db status",
	"env":        "putnami cloud env status",
}

// remoteStatusProbe is one entry that calls a Putnami service.
type remoteStatusProbe struct {
	id  string
	run func(ioctx IO) clicore.StatusNode
}

func cloudStatus(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx IO) error {
	switch sub := clicore.FirstPositional(args); sub {
	case "":
	case "help":
		return entryHelp(params, ioctx, "status", []map[string]string{
			{"command": "cloud status", "description": "one line per entry: ok, degraded, failing or unknown, and the command that shows more"},
			{"command": "cloud status --strict", "description": "exit 1 on degraded and unknown too, not only on failing; use it to gate CI"},
			{"command": "cloud status --env <env>", "description": "check only that environment on the env line (default: every declared environment)"},
			{"command": "cloud <entry> status", "description": "one entry in full: its metrics and its checks"},
		})
	default:
		return newError("cloud status takes no argument; got "+sub, ExitUsage)
	}
	if truthy(param(params, "provenance")) || truthy(param(params, "health")) {
		return newError("`putnami cloud status` is the one-line summary; run `putnami cloud env status --provenance` or `--health` for the deployment table", ExitUsage)
	}
	nodes := collectStatus(params, workspaceRoot, env, ioctx)
	return clicore.WriteStatusSynthesis(params, ioctx, nodes, func(node clicore.StatusNode) string { return statusMore[node.ID] })
}

// collectStatus reads every entry. The whoami line is read first, alone, so
// a stored session is refreshed once; the workspace context is then resolved
// once, so the workspace token is minted once. The other entries run in
// parallel, each bounded by statusProbeTimeout. The registries line is set up
// per machine and runs without a linked workspace; the workspace lines are
// unknown, with the reason, until the machine is signed in and linked.
func collectStatus(params map[string]any, workspaceRoot string, env map[string]string, ioctx IO) []clicore.StatusNode {
	params = workspaceStatusParams(params)
	whoami := runStatusProbes([]remoteStatusProbe{{"whoami", func(probeIO IO) clicore.StatusNode {
		return identitycli.WhoamiStatusNode(params, workspaceRoot, env, probeIO)
	}}}, ioctx)[0]

	session, _ := whoami.Child("whoami.session")
	link, _ := whoami.Child("whoami.workspace")
	var blocked error
	switch {
	case session.State != clicore.StatusOK:
		blocked = fmt.Errorf("needs a working sign-in")
	case link.State != clicore.StatusOK:
		blocked = fmt.Errorf("needs a linked workspace")
	default:
		if _, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx); err != nil {
			blocked = err
		}
	}

	builders := map[string]func(map[string]any, string, map[string]string, IO) clicore.StatusNode{
		"token":      TokenStatusNode,
		"packages":   distributioncli.PackagesStatusNode,
		"channels":   distributioncli.ChannelsStatusNode,
		"ci":         deliverycli.CIStatusNode,
		"cache":      deliverycli.CacheStatusNode,
		"source":     sourcecli.SourceStatusNode,
		"config":     configcli.ConfigStatusNode,
		"secrets":    configcli.SecretsStatusNode,
		"db":         datacli.DBStatusNode,
		"env":        runtimecli.EnvStatusNode,
		"registries": distributioncli.RegistriesStatusNode,
	}
	probes := make([]remoteStatusProbe, 0, len(statusEntries)-1)
	nodes := make([]clicore.StatusNode, len(statusEntries))
	nodes[0] = whoami
	indexes := make([]int, 0, len(statusEntries)-1)
	for index, id := range statusEntries[1:] {
		build := builders[id]
		if blocked != nil && id != "registries" {
			nodes[index+1] = clicore.UnknownStatus(id, id, blocked, statusMore[id])
			continue
		}
		probes = append(probes, remoteStatusProbe{id, func(probeIO IO) clicore.StatusNode {
			return build(params, workspaceRoot, env, probeIO)
		}})
		indexes = append(indexes, index+1)
	}
	for position, node := range runStatusProbes(probes, ioctx) {
		nodes[indexes[position]] = node
	}
	return nodes
}

// workspaceStatusParams drops the project the CLI adopts from the working
// directory: the synthesis covers the whole workspace wherever it runs.
func workspaceStatusParams(params map[string]any) map[string]any {
	scoped := make(map[string]any, len(params))
	for key, value := range params {
		if key != "app" && key != "appName" {
			scoped[key] = value
		}
	}
	return scoped
}

// runStatusProbes runs the probes in parallel. A probe that panics or does
// not answer within statusProbeTimeout is unknown; its result is dropped.
// Probes write nothing to stdout, and their stderr lines are serialized.
func runStatusProbes(probes []remoteStatusProbe, ioctx IO) []clicore.StatusNode {
	var stderr sync.Mutex
	probeIO := ioctx
	probeIO.Stdout = func(string) {}
	if ioctx.Stderr != nil {
		probeIO.Stderr = func(line string) {
			stderr.Lock()
			defer stderr.Unlock()
			ioctx.Stderr(line)
		}
	}
	base := ioctx.Context
	if base == nil {
		base = context.Background()
	}
	results := make([]clicore.StatusNode, len(probes))
	var wg sync.WaitGroup
	for index, probe := range probes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(base, statusProbeTimeout)
			defer cancel()
			callIO := probeIO
			callIO.Context = ctx
			answer := make(chan clicore.StatusNode, 1)
			go func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						answer <- clicore.UnknownStatus(probe.id, probe.id, fmt.Errorf("internal error: %v", recovered), statusMore[probe.id])
					}
				}()
				answer <- probe.run(callIO)
			}()
			select {
			case node := <-answer:
				results[index] = node
			case <-ctx.Done():
				results[index] = clicore.UnknownStatus(probe.id, probe.id,
					fmt.Errorf("no answer within %s", statusProbeTimeout), statusMore[probe.id])
			}
		}()
	}
	wg.Wait()
	return results
}
