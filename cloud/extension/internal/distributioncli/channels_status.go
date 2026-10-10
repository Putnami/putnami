package distributioncli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	ciproto "go.putnami.dev/protocol/ci"
	protocol "go.putnami.dev/protocol/distribution"
)

// The channels status: each channel is a check whose detail is the
// release set it points to, and each registry that reports on it is a check
// under it, up to date or behind.

// ChannelAnswer is the provider's answer for one channel, or why there is
// none.
type ChannelAnswer struct {
	Response *protocol.ChannelStatusResponse
	Err      error
}

// ChannelsStatus serves `putnami cloud channels status [<channel>] [--wait
// <duration>]`. Without a channel it reads every channel putnami.ci.json
// declares, once. With a channel it reads that one; --wait polls until every
// registry applied the head or the duration ends, and a channel still behind
// or unreadable then is failing. A registry behind without --wait is
// degraded: registries converge on their own after a release.
func ChannelsStatus(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	wait, err := channelWait(params, args)
	if err != nil {
		return err
	}
	positionals := clicore.Positionals(wait.rest)
	if len(positionals) > 1 {
		return clicore.NewError("cloud channels status takes at most one channel", clicore.ExitUsage)
	}
	if len(positionals) == 0 {
		if wait.duration > 0 {
			return clicore.NewError("cloud channels status --wait needs one channel", clicore.ExitUsage)
		}
		node, err := channelsStatusNode(params, workspaceRoot, env, ioctx)
		if err != nil {
			return err
		}
		return clicore.WriteStatus(params, ioctx, node)
	}
	channel := positionals[0]
	if err := requirePortableChannel(channel); err != nil {
		return err
	}
	namespace, err := ChannelNamespace(workspaceRoot)
	if err != nil {
		return clicore.WriteStatus(params, ioctx, clicore.UnknownStatus("channels."+channel, channel, err, ""))
	}
	provider := &channelProvider{params: params, workspaceRoot: workspaceRoot, env: env, ioctx: ioctx}
	deadline := time.Now().Add(wait.duration)
	for {
		response, err := provider.channelStatus(namespace, channel)
		var node clicore.StatusNode
		if err != nil {
			err = channelProviderError("status", namespace, channel, err)
			if clicore.ExitCode(err) == clicore.ExitAuth {
				return err
			}
			node = clicore.UnknownStatus("channels."+channel, channel, err, "")
		} else {
			node = ChannelStatusNodeFrom(channel, response)
			node.Metrics = []clicore.StatusMetric{
				clicore.CountMetric("registries_behind", "registries behind", channelRegistriesBehind(node), clicore.MetricHealth),
			}
		}
		if node.State == clicore.StatusOK || !time.Now().Before(deadline) {
			if node.State != clicore.StatusOK && wait.duration > 0 {
				// --wait gates a script: a channel not served when the wait
				// ends fails, whether a registry is behind or the provider
				// does not answer.
				node.State = clicore.StatusFailing
				if err != nil {
					node.Detail += "; still unreadable after " + wait.duration.String()
				} else {
					node.Detail += "; still behind after " + wait.duration.String()
				}
			}
			return clicore.WriteStatus(params, ioctx, node)
		}
		if err := sleepUntilNextPoll(ioctx, deadline); err != nil {
			return err
		}
	}
}

// ChannelsStatusNode is the channels status of the workspace: every channel
// putnami.ci.json declares, read once.
func ChannelsStatusNode(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) clicore.StatusNode {
	node, err := channelsStatusNode(params, workspaceRoot, env, ioctx)
	if err != nil {
		return clicore.UnknownStatus("channels", "channels", err, "putnami cloud login")
	}
	return node
}

// channelsStatusNode is ChannelsStatusNode, and it returns an auth failure
// before any channel was read as an error, so the command keeps its exit
// code.
func channelsStatusNode(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) (clicore.StatusNode, error) {
	channels, err := DeclaredChannels(workspaceRoot)
	if err != nil {
		return clicore.UnknownStatus("channels", "channels", err, ""), nil
	}
	if len(channels) == 0 {
		return ChannelsStatusNodeFrom("", nil, nil), nil
	}
	namespace, err := ChannelNamespace(workspaceRoot)
	if err != nil {
		return clicore.UnknownStatus("channels", "channels", err, ""), nil
	}
	provider := &channelProvider{params: params, workspaceRoot: workspaceRoot, env: env, ioctx: ioctx}
	answers := make(map[string]ChannelAnswer, len(channels))
	for index, channel := range channels {
		response, err := provider.channelStatus(namespace, channel)
		if err != nil {
			err = channelProviderError("status", namespace, channel, err)
			if index == 0 && clicore.ExitCode(err) == clicore.ExitAuth {
				return clicore.StatusNode{}, err
			}
		}
		answers[channel] = ChannelAnswer{Response: response, Err: err}
	}
	return ChannelsStatusNodeFrom(namespace, channels, answers), nil
}

// ChannelsStatusNodeFrom folds the answer of each declared channel. The node
// takes the worst channel.
func ChannelsStatusNodeFrom(namespace string, channels []string, answers map[string]ChannelAnswer) clicore.StatusNode {
	node := clicore.StatusNode{ID: "channels", Title: "channels"}
	if len(channels) == 0 {
		node.State = clicore.StatusOK
		node.Detail = "no channel declared in " + ciproto.Filename
		return node
	}
	behind := 0
	var notes []string
	for _, channel := range channels {
		answer := answers[channel]
		var child clicore.StatusNode
		if answer.Err != nil || answer.Response == nil {
			child = clicore.UnknownStatus("channels."+channel, channel, answer.Err, "")
		} else {
			child = ChannelStatusNodeFrom(channel, answer.Response)
		}
		behind += channelRegistriesBehind(child)
		switch {
		case child.State == clicore.StatusUnknown:
			notes = append(notes, channel+" not read")
		case child.State != clicore.StatusOK:
			notes = append(notes, channel+" behind")
		}
		node.Children = append(node.Children, child)
	}
	node.State = clicore.WorstStatus(node.ChildStates()...)
	node.Metrics = []clicore.StatusMetric{
		clicore.CountMetric("channels", "channels declared", len(channels), clicore.MetricUsage),
		clicore.CountMetric("registries_behind", "registries behind", behind, clicore.MetricHealth),
	}
	node.Detail = fmt.Sprintf("%d channels in namespace %s", len(channels), namespace)
	if len(notes) > 0 {
		node.Detail += "; " + strings.Join(notes, ", ")
	} else {
		node.Detail += ", every registry up to date"
	}
	return node
}

// ChannelStatusNodeFrom folds the answer for one channel: the head and its
// generation, and one check per registry that reports on it. A channel with a
// head that no registry reports on is behind too; otherwise `--wait` would
// succeed at once exactly when no registry can ever serve the head.
func ChannelStatusNodeFrom(channel string, response *protocol.ChannelStatusResponse) clicore.StatusNode {
	node := clicore.StatusNode{ID: "channels." + channel, Title: channel, State: clicore.StatusOK}
	if response == nil || response.Desired == nil {
		node.Detail = "no release yet"
		return node
	}
	desired := response.Desired
	node.Detail = fmt.Sprintf("%s, generation %d", desired.Ref.ID, desired.Generation)
	kinds := make([]string, 0, len(response.Observed))
	for kind := range response.Observed {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	var behind []string
	for _, kind := range kinds {
		observed := response.Observed[kind]
		registry := clicore.StatusNode{ID: node.ID + "." + kind, Title: kind, State: clicore.StatusOK,
			Detail: fmt.Sprintf("generation %d, up to date", observed)}
		if observed < desired.Generation {
			registry.State = clicore.StatusDegraded
			registry.Detail = fmt.Sprintf("generation %d of %d, behind", observed, desired.Generation)
			behind = append(behind, kind)
		}
		node.Children = append(node.Children, registry)
	}
	switch {
	case len(kinds) == 0:
		node.State = clicore.StatusDegraded
		node.Detail += "; no registry reports on it"
	case len(behind) > 0:
		node.State = clicore.StatusDegraded
		node.Detail += "; behind on " + strings.Join(behind, ", ")
	}
	if node.State != clicore.StatusOK {
		node.Fix = "putnami cloud channels status " + channel + " --wait 5m"
	}
	return node
}

// channelRegistriesBehind counts the registries of a channel node that have
// not applied its head. A head that no registry reports on counts as one.
func channelRegistriesBehind(node clicore.StatusNode) int {
	if node.State == clicore.StatusOK || node.State == clicore.StatusUnknown {
		return 0
	}
	if len(node.Children) == 0 {
		return 1
	}
	count := 0
	for _, registry := range node.Children {
		if registry.State != clicore.StatusOK {
			count++
		}
	}
	return count
}
