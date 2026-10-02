package lifecycle

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	ciproto "go.putnami.dev/protocol/ci"
	diag "go.putnami.dev/protocol/diagnostic"
	distribution "go.putnami.dev/protocol/distribution"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	providerext "go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// channelStatusPollInterval is how often `channel status --wait` asks the
// provider again. Registries converge asynchronously from the release facts, so
// the wait is a poll rather than a subscription (D32).
const channelStatusPollInterval = 2 * time.Second

// channelClient is the provider seam both channel commands use: one
// metadata-only move, one desired-versus-observed report, and the resolve that
// reads the target channel's current head.
type channelClient interface {
	Resolve(context.Context, *distribution.ResolveRequest) (*distribution.ResolveResponse, error)
	ChannelSet(context.Context, *distribution.ChannelSetRequest) (*distribution.ReleaseResponse, error)
	ChannelStatus(context.Context, *distribution.ChannelStatusRequest) (*distribution.ChannelStatusResponse, error)
}

// newChannelClient resolves the one installed release-set provider and binds a
// bounded client to it, carrying the same provider-only process environment the
// publish coordinator uses. Without that environment the nested provider call
// would run with the parent's ambient credential and without the capability
// marker that authorizes it.
//
// It deliberately does NOT answer which namespace to address: that is a property
// of the repository, not of the provider binary, and keeping the two apart is
// what lets the namespace rule be exercised for real while the provider stays a
// test double (channelNamespace below).
var newChannelClient = func(ctx context.Context, wsRoot string, cfg *wsproto.Config) (channelClient, error) {
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return nil, fmt.Errorf("load workspace: %w", err)
	}
	projectPaths := make([]string, len(ws.Projects))
	for index, project := range ws.Projects {
		projectPaths[index] = project.Path
	}
	discovered, err := providerext.DiscoverExtensionsDetailed(wsRoot, cfg, projectPaths)
	if err != nil {
		return nil, fmt.Errorf("discover release-set provider: %w", err)
	}
	provider, err := providerext.ResolveReservedProvider(discovered.Extensions, distribution.ProviderCommandName)
	if err != nil {
		return nil, fmt.Errorf("resolve release-set provider: %w", err)
	}
	if provider == nil {
		detail := fmt.Sprintf("install exactly one extension declaring %q", distribution.ProviderCommandName)
		if cause := discovered.ProviderCause(distribution.ProviderCommandName); cause != "" {
			detail += ". " + cause
		}
		return nil, fmt.Errorf("%w: %s", releaseset.ErrProviderAbsent, detail)
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate putnami executable: %w", err)
	}
	// The client starts `putnami cloud release-set`, a CLI that loads the
	// workspace's extensions.
	runcredential.MarkRepositoryCodeStarted("putnami cloud release-set")
	client := releaseset.NewClient(executable)
	client.Env = jobs.ReleaseSetProviderProcessEnv(ctx, provider)
	return client, nil
}

// channelNamespace is the provider identity these two commands address. It is
// the PUBLISHER's rule (jobs.DistributionNamespace), not the workspace's local
// name: a repository that declares `distribution.namespace` publishes every
// release set under it, so a promotion that asked about `ws.Name` addressed a
// namespace nothing had ever written and could only fail.
//
// cfg.Name is the same string workspace.Load would report as ws.Name — both come
// from wsproto.Load(wsRoot) — so reading it here avoids a second workspace load
// without changing the fallback.
func channelNamespace(wsRoot string, cfg *wsproto.Config) (string, error) {
	name := ""
	if cfg != nil {
		name = cfg.Name
	}
	namespace, err := jobs.DistributionNamespace(wsRoot, name)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(namespace) == "" {
		return "", cmderr.Usagef(
			"no release-set namespace: declare distribution.namespace in %s, or name the workspace",
			ciproto.Filename)
	}
	return namespace, nil
}

// ChannelSetFlags is `putnami channel set <channel> --from <channel|rs_id>`.
type ChannelSetFlags struct {
	// From names the set to point the channel at: an existing channel whose
	// head is re-released, or one immutable rs_ id.
	From string
	// Expected is the rs_ id the target channel must currently name. When it is
	// empty the command resolves the channel and uses its current head, so an
	// unattended move still fails closed against a concurrent writer.
	Expected string
}

// ChannelSet moves a channel to a set that already exists. It is promotion and
// rollback both: one provider call, no publisher, no build, no checkout (D2).
// The provider re-releases the named set on the target channel under the same
// compare-and-swap guarantees a publish gets.
func ChannelSet(ctx context.Context, wsRoot string, cfg *wsproto.Config, channel string, flags ChannelSetFlags) error {
	if err := requirePortableChannel(channel); err != nil {
		return err
	}
	source, err := parseChannelSource(flags.From)
	if err != nil {
		return err
	}
	namespace, err := channelNamespace(wsRoot, cfg)
	if err != nil {
		return err
	}
	client, err := newChannelClient(ctx, wsRoot, cfg)
	if err != nil {
		return err
	}
	expected, err := resolveChannelExpectation(ctx, client, namespace, channel, flags.Expected)
	if err != nil {
		return err
	}
	request := &distribution.ChannelSetRequest{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       namespace,
		Channel:         channel,
		Expected:        expected,
		From:            source,
	}
	response, err := client.ChannelSet(ctx, request)
	if err != nil {
		return channelProviderError("set", namespace, channel, err)
	}
	head := response.Current[channel]
	if response.Outcome == distribution.ReleaseOutcomeConflict || head == nil {
		return fmt.Errorf("channel %s moved under this command; re-run it against the current head", channel)
	}
	iox.Fprintf(os.Stdout, "  %s -> %s (generation %d)\n", channel, head.Ref.ID, head.Generation)
	return nil
}

// ChannelStatusFlags is `putnami channel status <channel> [--wait <duration>]`.
type ChannelStatusFlags struct {
	// Wait bounds how long the command polls for every registry to reach the
	// accepted head. Zero reports once.
	Wait time.Duration
}

// ChannelStatus reports desired versus observed: the head the provider
// accepted, and the generation each registry has applied. A registry behind the
// accepted generation exits non-zero, so a pipeline that must not proceed
// before a channel is served stops instead of counting a silent zero (D32).
func ChannelStatus(ctx context.Context, wsRoot string, cfg *wsproto.Config, channel string, flags ChannelStatusFlags) error {
	if err := requirePortableChannel(channel); err != nil {
		return err
	}
	namespace, err := channelNamespace(wsRoot, cfg)
	if err != nil {
		return err
	}
	client, err := newChannelClient(ctx, wsRoot, cfg)
	if err != nil {
		return err
	}
	request := &distribution.ChannelStatusRequest{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       namespace,
		Channel:         channel,
	}
	deadline := time.Now().Add(flags.Wait)
	for {
		response, err := client.ChannelStatus(ctx, request)
		if err != nil {
			return channelProviderError("status", namespace, channel, err)
		}
		behind := printChannelStatus(channel, response)
		if len(behind) == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("channel %s is not served by %s yet", channel, strings.Join(behind, ", "))
		}
		if err := sleepUntilNextPoll(ctx, deadline); err != nil {
			return err
		}
	}
}

// printChannelStatus renders one report and returns the registries whose
// applied generation is behind the accepted one.
func printChannelStatus(channel string, response *distribution.ChannelStatusResponse) []string {
	if response == nil || response.Desired == nil {
		iox.Fprintf(os.Stdout, "  %s: no head\n", channel)
		return nil
	}
	desired := response.Desired
	iox.Fprintf(os.Stdout, "  %s desired %s (generation %d)\n", channel, desired.Ref.ID, desired.Generation)
	kinds := make([]string, 0, len(response.Observed))
	for kind := range response.Observed {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	var behind []string
	for _, kind := range kinds {
		observed := response.Observed[kind]
		state := "up to date"
		if observed < desired.Generation {
			state = "behind"
			behind = append(behind, kind)
		}
		iox.Fprintf(os.Stdout, "    %-10s generation %d (%s)\n", kind, observed, state)
	}
	if len(response.Observed) == 0 {
		// A publisher that finds no subscriber reports it instead of counting a
		// silent zero (D32). Treat the absence as unconverged as well as naming it:
		// otherwise `channel status --wait` exits successfully on its first poll
		// precisely when no registry can ever serve the accepted head.
		iox.Fprintln(os.Stdout, "    no registry reports on this channel")
		behind = append(behind, "any registry")
	}
	return behind
}

// sleepUntilNextPoll waits for the poll interval, or until the deadline or the
// invocation context ends, whichever comes first.
func sleepUntilNextPoll(ctx context.Context, deadline time.Time) error {
	delay := min(channelStatusPollInterval, time.Until(deadline))
	if delay <= 0 {
		return nil
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

// resolveChannelExpectation is the compare-and-swap expectation of the move: the
// id the caller stated, or the channel's current head read now. A channel with
// no head asserts exactly that, which is how an immutable channel is created.
func resolveChannelExpectation(
	ctx context.Context,
	client channelClient,
	namespace, channel, stated string,
) (*distribution.ReleaseSetRef, error) {
	if trimmed := strings.TrimSpace(stated); trimmed != "" {
		if diagnostics := distribution.ValidateReleaseSetID("--expected", trimmed); diag.HasErrors(diagnostics) {
			return nil, cmderr.Usagef("--expected %s is not a release-set id (want rs_<64 lowercase hex>)", trimmed)
		}
		// The two spellings of a ref carry the same SHA-256 hex by
		// construction, so naming the id names the digest too.
		return &distribution.ReleaseSetRef{
			ID:     trimmed,
			Digest: "sha256:" + strings.TrimPrefix(trimmed, "rs_"),
		}, nil
	}
	request := &distribution.ResolveRequest{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       namespace,
		Channels:        []string{channel},
	}
	response, err := client.Resolve(ctx, request)
	if err != nil {
		return nil, channelProviderError("resolve", namespace, channel, err)
	}
	if response == nil {
		return nil, fmt.Errorf("resolve channel %s in namespace %s: provider returned no response",
			channel, namespace)
	}
	head := response.Heads[channel]
	if head == nil {
		return nil, nil
	}
	ref := head.Ref
	return &ref, nil
}

// channelProviderError names WHAT was asked of the provider when the provider
// answers with a failure.
//
// It is deliberately not an error CLASS: the distribution protocol defines
// diagnostic codes for release-set DOCUMENTS (distribution.ErrorCode*) but no
// exit-status vocabulary for the provider PROCESS, so `exit status 4` carries no
// portable meaning to translate and inventing one here would put a name on a
// number only one provider assigns. Provider stderr stays out for the reason the
// SDK states at releaseset/client.go: it is human-owned and may carry
// credentials.
//
// What an operator can act on is the request that failed — and the namespace is
// the part of it nobody typed, so it is the part that used to be invisible when
// it was wrong.
func channelProviderError(operation, namespace, channel string, err error) error {
	return fmt.Errorf("%s channel %s in namespace %s: %w", operation, channel, namespace, err)
}

// parseChannelSource reads `--from`: an rs_ id names the immutable set
// directly, anything else names a channel whose current head is re-released.
func parseChannelSource(from string) (distribution.ChannelSource, error) {
	trimmed := strings.TrimSpace(from)
	if trimmed == "" {
		return distribution.ChannelSource{}, cmderr.Usagef("channel set needs --from <channel|rs_id>")
	}
	if strings.HasPrefix(trimmed, "rs_") {
		return distribution.ChannelSource{ReleaseID: trimmed}, nil
	}
	if err := requirePortableChannel(trimmed); err != nil {
		return distribution.ChannelSource{}, err
	}
	return distribution.ChannelSource{Channel: trimmed}, nil
}

// requirePortableChannel refuses a name outside the portable alphabet before
// any provider call. The provider refuses it too, but a name that is not a
// valid npm dist-tag, Go query, OCI tag and put channel at once is a typo far
// more often than a policy question (D33).
func requirePortableChannel(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return cmderr.Usagef("a channel name is required")
	}
	if !distribution.IsPortableChannel(trimmed) {
		return cmderr.Usagef("%s is not a portable channel name; a channel matches %s", trimmed, distribution.ChannelPattern)
	}
	return nil
}
