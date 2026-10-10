package distributioncli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	ciproto "go.putnami.dev/protocol/ci"
	diag "go.putnami.dev/protocol/diagnostic"
	protocol "go.putnami.dev/protocol/distribution"
)

// `putnami cloud channels set|status`. They replace the
// framework's `putnami channel set|status`, which spawned `putnami cloud
// release-set` for every provider call. Here the calls run in process through
// the same validation, credential and HTTP path that command uses, so the
// provider exchange is identical and no second process starts.

// channelPollInterval is how often `channels status --wait` asks the provider
// again. Registries converge asynchronously from the release facts, so the wait
// is a poll. Tests shorten it.
var channelPollInterval = 2 * time.Second

// channelProvider is one resolved provider session: the Put endpoint and the
// bearer, resolved once per command and reused by every call it makes.
type channelProvider struct {
	params        map[string]any
	workspaceRoot string
	env           map[string]string
	ioctx         clicore.IO
	credential    *resolvedRegistryToken
}

// exchange runs one release-set operation: it validates the request, calls
// put-server, validates the answer, and decodes it into response.
func (p *channelProvider) exchange(operation string, request, response any) error {
	raw, err := json.Marshal(request)
	if err != nil {
		return clicore.NewError("encode release-set "+operation+" request: "+err.Error(), clicore.ExitAPI)
	}
	prepared, err := prepareReleaseSetRequest(operation, raw)
	if err != nil {
		return err
	}
	if p.credential == nil {
		credential, err := resolveReleaseSetProviderCredential(p.params, p.workspaceRoot, p.env, p.ioctx)
		if err != nil {
			return err
		}
		p.credential = &credential
	}
	answer, err := callReleaseSetProvider(p.ioctx, p.credential.Endpoint, p.credential.Token, prepared.operation, prepared.body)
	if err != nil {
		return err
	}
	output, err := prepared.consume(answer)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(output, response); err != nil {
		return clicore.NewError("decode release-set "+operation+" response: "+err.Error(), clicore.ExitAPI)
	}
	return nil
}

func (p *channelProvider) resolve(namespace, channel string) (*protocol.ResolveResponse, error) {
	response := &protocol.ResolveResponse{}
	err := p.exchange(protocol.ResolveCommand, &protocol.ResolveRequest{
		ProtocolVersion: protocol.ProtocolVersion,
		Namespace:       namespace,
		Channels:        []string{channel},
	}, response)
	return response, err
}

func (p *channelProvider) channelSet(request *protocol.ChannelSetRequest) (*protocol.ReleaseResponse, error) {
	response := &protocol.ReleaseResponse{}
	return response, p.exchange(protocol.ChannelSetCommand, request, response)
}

func (p *channelProvider) channelStatus(namespace, channel string) (*protocol.ChannelStatusResponse, error) {
	response := &protocol.ChannelStatusResponse{}
	err := p.exchange(protocol.ChannelStatusCommand, &protocol.ChannelStatusRequest{
		ProtocolVersion: protocol.ProtocolVersion,
		Namespace:       namespace,
		Channel:         channel,
	}, response)
	return response, err
}

// ChannelsSet serves `putnami cloud channels set <channel> --from
// <channel|rs_id> [--expected <rs_id>]`. It moves a channel to a release set
// that already exists: promotion and rollback both. The move is a
// compare-and-swap against the head the channel names now, so a concurrent
// writer makes it fail instead of being overwritten.
func ChannelsSet(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	positionals := clicore.Positionals(args)
	if len(positionals) != 1 {
		return clicore.NewError("cloud channels set takes one channel: run `putnami cloud channels set <channel> --from <channel|rs_id>`", clicore.ExitUsage)
	}
	channel := positionals[0]
	if err := requirePortableChannel(channel); err != nil {
		return err
	}
	source, err := parseChannelSource(clicore.StringParam(params, "from"))
	if err != nil {
		return err
	}
	namespace, err := ChannelNamespace(workspaceRoot)
	if err != nil {
		return err
	}
	provider := &channelProvider{params: params, workspaceRoot: workspaceRoot, env: env, ioctx: ioctx}
	expected, err := resolveChannelExpectation(provider, namespace, channel, clicore.StringParam(params, "expected"))
	if err != nil {
		return err
	}
	response, err := provider.channelSet(&protocol.ChannelSetRequest{
		ProtocolVersion: protocol.ProtocolVersion,
		Namespace:       namespace,
		Channel:         channel,
		Expected:        expected,
		From:            source,
	})
	if err != nil {
		return channelProviderError("set", namespace, channel, err)
	}
	head := response.Current[channel]
	if response.Outcome == protocol.ReleaseOutcomeConflict || head == nil {
		return clicore.NewError(fmt.Sprintf("channel %s moved while this command ran; run it again against the current head", channel), clicore.ExitAPI)
	}
	clicore.WriteResult(map[string]any{
		"namespace":  namespace,
		"channel":    channel,
		"releaseSet": head.Ref.ID,
		"generation": head.Generation,
	}, params, ioctx, fmt.Sprintf("  %s -> %s (generation %d)", channel, head.Ref.ID, head.Generation))
	return nil
}

// ChannelNamespace is the namespace the channel commands address: the
// `distribution.namespace` putnami.ci.json declares, else the workspace name.
// It is the publisher's rule, so a promotion reads the namespace every publish
// wrote. An unreadable or invalid putnami.ci.json is an error, never a silent
// fallback: a guessed namespace would move a channel nobody publishes to.
func ChannelNamespace(workspaceRoot string) (string, error) {
	document, found, err := readCIDocument(workspaceRoot)
	if err != nil {
		return "", err
	}
	if found && document.Distribution != nil && strings.TrimSpace(document.Distribution.Namespace) != "" {
		return document.Distribution.Namespace, nil
	}
	name, err := workspaceName(workspaceRoot)
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", clicore.NewError("no release-set namespace: declare distribution.namespace in "+ciproto.Filename+", or name the workspace", clicore.ExitUsage)
	}
	return name, nil
}

// DeclaredChannels lists, sorted, every portable channel putnami.ci.json names
// outside its rules: the distribution channels and the channels environments,
// variants and workload rules follow. Rules may publish templated channels
// (one per pull request), which are not a fixed set and stay out.
func DeclaredChannels(workspaceRoot string) ([]string, error) {
	document, found, err := readCIDocument(workspaceRoot)
	if err != nil || !found {
		return nil, err
	}
	seen := map[string]bool{}
	add := func(channel string) {
		channel = strings.TrimSpace(channel)
		if channel != "" && protocol.IsPortableChannel(channel) {
			seen[channel] = true
		}
	}
	if document.Distribution != nil {
		for channel := range document.Distribution.Channels {
			add(channel)
		}
	}
	for _, environment := range document.Envs {
		add(environment.Channel)
		for _, variant := range environment.Variants {
			add(variant.Channel)
		}
		for _, rule := range environment.Workloads {
			add(rule.Channel)
		}
	}
	channels := make([]string, 0, len(seen))
	for channel := range seen {
		channels = append(channels, channel)
	}
	sort.Strings(channels)
	return channels, nil
}

// readCIDocument parses putnami.ci.json at the workspace root. A missing file
// is not an error; found reports whether it exists.
func readCIDocument(workspaceRoot string) (ciproto.Document, bool, error) {
	data, err := os.ReadFile(filepath.Join(workspaceRoot, ciproto.Filename))
	if errors.Is(err, os.ErrNotExist) {
		return ciproto.Document{}, false, nil
	}
	if err != nil {
		return ciproto.Document{}, false, clicore.NewError("read "+ciproto.Filename+": "+err.Error(), clicore.ExitUsage)
	}
	document, err := ciproto.Parse(data)
	if err != nil {
		return ciproto.Document{}, false, clicore.NewError("parse "+ciproto.Filename+": "+err.Error(), clicore.ExitUsage)
	}
	return document, true, nil
}

// workspaceName reads the `name` of putnami.workspace.json, else putnami.json:
// the same field the framework reads as the workspace name.
func workspaceName(workspaceRoot string) (string, error) {
	path := clicore.ManifestPath(workspaceRoot)
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", clicore.NewError("read "+filepath.Base(path)+": "+err.Error(), clicore.ExitUsage)
	}
	var manifest struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", clicore.NewError("parse "+filepath.Base(path)+": "+err.Error(), clicore.ExitUsage)
	}
	return strings.TrimSpace(manifest.Name), nil
}

// waitOption is the parsed --wait of `channels status`.
type waitOption struct {
	duration time.Duration
	// rest is argv without the --wait flag and its value.
	rest []string
}

// channelWait reads --wait <duration> or --wait=<duration>. `wait` is a
// boolean flag elsewhere in the CLI, so the shared parser would take the
// duration for a positional; the value is read from argv here and removed.
func channelWait(params map[string]any, args []string) (waitOption, error) {
	option := waitOption{rest: make([]string, 0, len(args))}
	value, found := "", false
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case strings.HasPrefix(arg, "--wait="):
			value, found = strings.TrimPrefix(arg, "--wait="), true
		case arg == "--wait":
			found = true
			if index+1 < len(args) && !strings.HasPrefix(args[index+1], "--") {
				value = args[index+1]
				index++
			}
		default:
			option.rest = append(option.rest, arg)
		}
	}
	if !found {
		if raw, ok := params["wait"].(string); ok && strings.TrimSpace(raw) != "" && raw != "true" {
			value, found = raw, true
		}
	}
	if !found {
		return option, nil
	}
	if value == "" {
		return option, clicore.NewError("cloud channels status --wait needs a duration, for example --wait 5m", clicore.ExitUsage)
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration < 0 {
		return option, clicore.NewError("cloud channels status --wait takes a duration such as 30s or 5m; got "+value, clicore.ExitUsage)
	}
	option.duration = duration
	return option, nil
}

// sleepUntilNextPoll waits one poll interval, or until the deadline or the
// command's context ends.
func sleepUntilNextPoll(ioctx clicore.IO, deadline time.Time) error {
	delay := min(channelPollInterval, time.Until(deadline))
	if delay <= 0 {
		return nil
	}
	ctx := ioctx.Context
	if ctx == nil {
		ctx = context.Background()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// resolveChannelExpectation is the compare-and-swap expectation of a move: the
// id the caller stated, or the channel's head read now. A channel with no head
// expects exactly that, which is how a new channel is created.
func resolveChannelExpectation(provider *channelProvider, namespace, channel, stated string) (*protocol.ReleaseSetRef, error) {
	if trimmed := strings.TrimSpace(stated); trimmed != "" {
		if diagnostics := protocol.ValidateReleaseSetID("--expected", trimmed); diag.HasErrors(diagnostics) {
			return nil, clicore.NewError("--expected "+trimmed+" is not a release-set id (want rs_<64 lowercase hex>)", clicore.ExitUsage)
		}
		// The id and the digest carry the same SHA-256 hex by construction.
		return &protocol.ReleaseSetRef{ID: trimmed, Digest: "sha256:" + strings.TrimPrefix(trimmed, "rs_")}, nil
	}
	response, err := provider.resolve(namespace, channel)
	if err != nil {
		return nil, channelProviderError("resolve", namespace, channel, err)
	}
	head := response.Heads[channel]
	if head == nil {
		return nil, nil
	}
	ref := head.Ref
	return &ref, nil
}

// channelProviderError names what was asked of the provider. The namespace is
// the part nobody typed, so it is the part worth showing.
func channelProviderError(operation, namespace, channel string, err error) error {
	return clicore.NewError(fmt.Sprintf("%s channel %s in namespace %s: %v", operation, channel, namespace, err), clicore.ExitCode(err))
}

// parseChannelSource reads --from: an rs_ id names one immutable set, any
// other value names a channel whose current head is released again.
func parseChannelSource(from string) (protocol.ChannelSource, error) {
	trimmed := strings.TrimSpace(from)
	if trimmed == "" {
		return protocol.ChannelSource{}, clicore.NewError("cloud channels set needs --from <channel|rs_id>", clicore.ExitUsage)
	}
	if strings.HasPrefix(trimmed, "rs_") {
		return protocol.ChannelSource{ReleaseID: trimmed}, nil
	}
	if err := requirePortableChannel(trimmed); err != nil {
		return protocol.ChannelSource{}, err
	}
	return protocol.ChannelSource{Channel: trimmed}, nil
}

// requirePortableChannel refuses a name outside the portable alphabet before
// any provider call: a name that is not at once a valid npm dist-tag, Go
// query, OCI tag and put channel is a typo far more often than a policy
// question.
func requirePortableChannel(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return clicore.NewError("a channel name is required", clicore.ExitUsage)
	}
	if !protocol.IsPortableChannel(trimmed) {
		return clicore.NewError(fmt.Sprintf("%s is not a portable channel name; a channel matches %s", trimmed, protocol.ChannelPattern), clicore.ExitUsage)
	}
	return nil
}
