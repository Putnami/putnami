package cloudcli

import (
	"fmt"
	"slices"
	"strings"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	configcli "go.putnami.dev/cloud/extension/internal/configcli"
	datacli "go.putnami.dev/cloud/extension/internal/datacli"
	deliverycli "go.putnami.dev/cloud/extension/internal/deliverycli"
	distributioncli "go.putnami.dev/cloud/extension/internal/distributioncli"
	runtimecli "go.putnami.dev/cloud/extension/internal/runtimecli"
	sourcecli "go.putnami.dev/cloud/extension/internal/sourcecli"
)

// The public Cloud CLI is flat: one root entry per thing the user knows
// Domains stay an internal code boundary, so an entry may
// route its verbs to several domain libraries. Every name the root carried
// before keeps working as a hidden alias; machine commands stay callable and
// hidden; platform maintenance commands live in the operator CLI.

// publicEntry is one root entry of `putnami cloud`.
type publicEntry struct {
	Name        string
	Usage       string
	Description string
}

// publicEntries is the whole public root, in the order help prints it.
var publicEntries = []publicEntry{
	{"login", "login", "sign in with your browser"},
	{"logout", "logout", "sign out and clear the stored credentials"},
	{"whoami", "whoami", "who you are signed in as, and the linked workspace"},
	{"setup", "setup", "link this repository to a Putnami Cloud workspace"},
	{"status", "status [--strict]", "one line per entry below that has a status: ok, degraded, failing or unknown"},
	{"token", "token status | [<target>] | create | list | revoke", "mint a bearer for a script (cache, npm, go, oci, put), or manage machine tokens"},
	{"registries", "registries status | setup | logout", "the npm, Go, OCI and put registries on this machine"},
	{"packages", "packages status | namespaces | grants | mirrors | copy | retag | revert | publish", "packages and images: namespaces, read grants, public mirrors, image moves"},
	{"channels", "channels status | set | follow", "publish a channel, or follow one"},
	{"ci", "ci status | list | view | logs | … | report | init | validate", "CI runs, recovery, and the putnami.ci.json contract"},
	{"cache", "cache status | disable", "the remote build cache"},
	{"source", "source status | connect | disconnect", "the GitHub repository linked to the workspace"},
	{"config", "config status | show | validate | drift | put | publish", "project configuration"},
	{"secrets", "secrets status | list | set | reveal | delete", "workspace and project secrets"},
	{"db", "db status | list | info | connect | grant | migrations", "databases: access grants, a local bridge, migrations"},
	{"env", "env status | doctor | enable", "environments: what runs where, and their prerequisites"},
	{"deploy", "deploy status <release> | publish-v2 --request-file <path>", "show one release, or submit a release set"},
	{"logs", "logs", "query a project's logs"},
	{"traces", "traces", "query a project's traces"},
	{"metrics", "metrics", "query a project's metrics"},
}

// hiddenAliases maps each retired root name to the entry that replaced it.
// The alias still runs, and the CLI's own help never lists it. The framework
// still lists it in `putnami cloud --help`, because the extension manifest has
// no hidden field.
var hiddenAliases = map[string]string{
	"tokens":            "token create|list|revoke",
	"distribution":      "packages namespaces|grants|mirrors",
	"oci":               "packages copy|retag|revert",
	"publish-archives":  "packages publish",
	"track":             "channels follow",
	"report":            "ci report",
	"validate-config":   "config validate",
	"publish-config":    "config publish",
	"publish-migration": "db migrations publish",
}

// machineCommands are called by the engine, a provider seam, or another
// command. They stay callable and never appear in help. Their names are
// protocol: do not rename them.
var machineCommands = []string{
	"install", "release-set", "registry-token", "image-layers", "publish-provider",
	"cache-provider", "credential-provider", "session-reporter", "log-reporter", "shell-test",
	"validate-config-project", "package-config-member", "package-site-content",
	"validate-site-content", "publish-site-content", "publish-deployment",
}

// operatorCommands are platform maintenance commands that left the public CLI
// for `putnami operator`. The public CLI answers them with a
// usage error that names the new command.
var operatorCommands = map[string]string{
	"sql-proxy":   "putnami operator sql-proxy",
	"publish-doc": "putnami operator publish-doc",
}

// selfHelpEntries answer `help` with their own verbs.
var selfHelpEntries = map[string]bool{
	"status": true, "token": true, "packages": true, "channels": true, "ci": true, "cache": true,
	"source": true, "config": true, "secrets": true, "db": true, "env": true,
}

// rootHelpArgs answers `putnami cloud <name> help` before any handler runs, so
// asking for help never acts: `logout help` does not sign you out,
// `registries logout help` keeps every registry credential and `deploy help`
// deploys nothing. A bare `help` counts at any position after the entry, so a
// positional value spelled `help` also reads as a help request. A hidden alias
// names the entry that replaced it, and an entry without verbs prints its
// usage line. An entry with verbs gets `help` as its first positional, so
// --help and a later `help` reach its own help too. answered reports that the
// help is already printed.
func rootHelpArgs(params map[string]any, ioctx IO, command string, args []string) ([]string, bool, error) {
	if !slices.Contains(clicore.Positionals(args), "help") && !clicore.BoolParam(params, false, "help") {
		return args, false, nil
	}
	if replacement, ok := hiddenAliases[command]; ok {
		aliasHelp(params, ioctx, command, replacement)
		return args, true, nil
	}
	if selfHelpEntries[command] {
		if clicore.FirstPositional(args) != "help" {
			args = append([]string{"help"}, args...)
		}
		return args, false, nil
	}
	for _, entry := range publicEntries {
		if entry.Name == command {
			return args, true, entryHelp(params, ioctx, entry.Name, []map[string]string{
				{"command": "cloud " + entry.Usage, "description": entry.Description},
			})
		}
	}
	return args, false, nil
}

// aliasHelp answers help on a hidden alias with the entry that replaced it.
func aliasHelp(params map[string]any, ioctx IO, alias, replacement string) {
	if clicore.StructuredOutput(params) {
		writeResult(map[string]any{"alias": "cloud " + alias, "replacement": "cloud " + replacement}, params, ioctx, "")
		return
	}
	entry := strings.Fields(replacement)[0]
	ioctx.Stdout(fmt.Sprintf("`putnami cloud %s` is an older name of `putnami cloud %s`; it still runs. Run `putnami cloud %s help` for the verbs.", alias, replacement, entry))
}

// movedToOperator is the answer of the public CLI to a command that moved to
// the operator CLI.
func movedToOperator(command, operator string) error {
	return newError(fmt.Sprintf("`putnami cloud %s` is a platform operator command; run `%s` from the @putnami/operator extension", command, operator), ExitUsage)
}

// routeToken serves `cloud token`: `token [<target>]` mints a bearer and
// `token create|list|revoke` manages workspace machine tokens.
func routeToken(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx IO) error {
	switch sub := clicore.FirstPositional(args); sub {
	case "status", "create", "list", "revoke":
		return machineTokens(params, args, workspaceRoot, env, ioctx)
	case "help":
		return entryHelp(params, ioctx, "token", []map[string]string{
			{"command": "cloud token", "description": "mint a workspace bearer for a script"},
			{"command": "cloud token <target>", "description": "mint a bearer for one target: cache, npm, go, oci or put"},
			{"command": "cloud token status [--strict]", "description": "check the workspace machine tokens: expired but not revoked, or expiring within 7 days"},
			{"command": "cloud token create --name <name> --scopes <s,...>", "description": "create a workspace machine token (pkt_*); the raw token is shown once"},
			{"command": "cloud token list", "description": "list the workspace machine tokens (never the secret)"},
			{"command": "cloud token revoke <id|name>", "description": "revoke a workspace machine token"},
		})
	case "":
		return workspaceToken(params, workspaceRoot, env, ioctx)
	default:
		if existing := strings.TrimSpace(stringParam(params, "for")); existing != "" && existing != sub {
			return newError(fmt.Sprintf("cloud token takes one target; got %q and --for %q", sub, existing), ExitUsage)
		}
		params["for"] = sub
		return workspaceToken(params, workspaceRoot, env, ioctx)
	}
}

// routePackages serves `cloud packages`: the status of the namespace
// bindings, Distribution's namespaces, grants and mirrors, the OCI registry's
// image moves, and archive publication.
func routePackages(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx IO) error {
	switch sub := clicore.FirstPositional(args); sub {
	case "", "help":
		return entryHelp(params, ioctx, "packages", []map[string]string{
			{"command": "cloud packages status [<protocol>]", "description": "check that the linked workspace binds each package namespace this checkout needs"},
			{"command": "cloud packages namespaces activate|list", "description": "bind a package namespace to the linked workspace, or list the bindings"},
			{"command": "cloud packages grants create|list|revoke", "description": "grant another workspace read access to the release history"},
			{"command": "cloud packages mirrors add|list|remove|rotate-credential", "description": "copy public packages to an npm, OCI or GitHub Release target"},
			{"command": "cloud packages copy <src-ref> <dst-ref>", "description": "copy an image on the Putnami OCI registry, server-side"},
			{"command": "cloud packages retag <ref> <tag...>", "description": "add tags to an image, server-side"},
			{"command": "cloud packages revert <repo>:<tag>", "description": "move a tag back to its previous image"},
			{"command": "cloud packages publish [<project>]", "description": "publish a project's pre-built archives as one immutable version"},
		})
	case "status":
		return distributioncli.PackagesStatus(params, clicore.DropFirstPositional(args), workspaceRoot, env, ioctx)
	case "namespaces", "bindings":
		return distributioncli.Distribution(params, clicore.ReplaceFirstPositional(args, "bindings"), workspaceRoot, env, ioctx)
	case "grants", "mirrors":
		return distributioncli.Distribution(params, args, workspaceRoot, env, ioctx)
	case "copy", "retag", "revert":
		return distributioncli.OCI(params, args, workspaceRoot, env, ioctx)
	case "publish":
		return publishArchives(params, clicore.DropFirstPositional(args), workspaceRoot, env, ioctx)
	default:
		return newError("unknown cloud packages command: "+sub+"; expected namespaces, grants, mirrors, copy, retag, revert, publish or status", ExitUsage)
	}
}

// publishArchives publishes a project's archives and emits the native
// release-set member the engine reads.
func publishArchives(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx IO) error {
	published, err := distributioncli.PublishArchivesWithResult(params, args, workspaceRoot, env, ioctx)
	if err != nil || published == nil {
		return err
	}
	return emitArchivePublishedMember(ioctx, published)
}

// routeChannels serves `cloud channels`: set and status talk to the
// release-set provider, follow changes what the workspace's CI tracks.
func routeChannels(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx IO) error {
	switch sub := clicore.FirstPositional(args); sub {
	case "", "help":
		return entryHelp(params, ioctx, "channels", []map[string]string{
			{"command": "cloud channels set <channel> --from <channel|rs_id>", "description": "point a channel at a release set that already exists: promotion and rollback"},
			{"command": "cloud channels status [<channel>] [--wait <duration>]", "description": "show the head of a channel and how far each registry applied it; without a name, every channel putnami.ci.json declares"},
			{"command": "cloud channels follow <namespace> <stable|canary|rs_id>", "description": "make the workspace's CI follow a producer's channel, or pin one release set"},
		})
	case "set":
		return distributioncli.ChannelsSet(params, clicore.DropFirstPositional(args), workspaceRoot, env, ioctx)
	case "status":
		return distributioncli.ChannelsStatus(params, clicore.DropFirstPositional(args), workspaceRoot, env, ioctx)
	case "follow":
		if err := adoptFollowPositionals(params, clicore.Positionals(clicore.DropFirstPositional(args))); err != nil {
			return err
		}
		return deliverycli.Track(params, nil, workspaceRoot, env, ioctx)
	default:
		return newError("unknown cloud channels command: "+sub+"; expected set, status or follow", ExitUsage)
	}
}

// adoptFollowPositionals maps `channels follow <namespace> <channel|rs_id>`
// onto the --namespace, --channel and --release-set flags `cloud track` reads.
func adoptFollowPositionals(params map[string]any, positionals []string) error {
	if len(positionals) > 2 {
		return newError("cloud channels follow takes <namespace> and one <channel|rs_id>", ExitUsage)
	}
	if len(positionals) > 0 && stringParam(params, "namespace") == "" {
		params["namespace"] = positionals[0]
	}
	if len(positionals) == 2 && stringParam(params, "channel") == "" && stringParam(params, "release-set") == "" {
		if strings.HasPrefix(positionals[1], "rs_") {
			params["release-set"] = positionals[1]
		} else {
			params["channel"] = positionals[1]
		}
	}
	return nil
}

// routeSource serves `cloud source`. The break-glass binding by numeric
// GitHub ids moved to the operator CLI.
func routeSource(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx IO) error {
	switch sub := clicore.FirstPositional(args); sub {
	case "bind", "unbind":
		return movedToOperator("source "+sub, "putnami operator source "+sub)
	default:
		return sourcecli.Source(params, args, workspaceRoot, env, ioctx)
	}
}

// routeConfig serves `cloud config`. `show` absorbs the retired
// `config <project>`, `config list` and `config resolve` through options;
// validate and publish are the retired validate-config and publish-config.
func routeConfig(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx IO) error {
	switch clicore.FirstPositional(args) {
	case "validate":
		return validateConfig(params, clicore.DropFirstPositional(args), workspaceRoot, ioctx)
	case "publish":
		return publishConfig(params, clicore.DropFirstPositional(args), workspaceRoot, env, ioctx)
	case "show":
		showArgs, err := configShowArgs(params, args)
		if err != nil {
			return err
		}
		return configcli.Config(params, showArgs, workspaceRoot, env, ioctx)
	default:
		return configcli.Config(params, args, workspaceRoot, env, ioctx)
	}
}

// configShowArgs selects the view `config show` prints from its options, and
// rewrites the verb to the handler that renders it.
//
//	--schema, --key, --secret-keys, --reveal-secrets  the schema-aware views of `config <project>`
//	--declared                                       every declared key and whether it is set
//	--keys                                           the resolved key names (`config list`)
//	--metadata                                       the merge metadata (`config resolve`)
//	no option                                        the resolved values
func configShowArgs(params map[string]any, args []string) ([]string, error) {
	inspect := truthy(param(params, "schema")) || truthy(param(params, "secret-keys", "secretKeys")) ||
		truthy(param(params, "reveal-secrets", "revealSecrets")) || stringParam(params, "key") != "" ||
		truthy(param(params, "declared"))
	switch {
	case inspect:
		// `config <project>` takes the project as its first positional, or the
		// project of the current directory.
		rest := clicore.DropFirstPositional(args)
		if project := clicore.FirstPositional(rest); project != "" {
			params["app"] = project
			return clicore.DropFirstPositional(rest), nil
		}
		if stringParam(params, "app", "appName") == "" {
			return nil, newError("cloud config show needs a project: run `putnami cloud config show <project>` or run it from the project directory", ExitUsage)
		}
		return rest, nil
	case truthy(param(params, "keys")):
		return clicore.ReplaceFirstPositional(args, "list"), nil
	case truthy(param(params, "metadata")):
		return clicore.ReplaceFirstPositional(args, "resolve"), nil
	default:
		return args, nil
	}
}

// routeDB serves `cloud db`; `db migrations publish` is the retired
// publish-migration.
func routeDB(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx IO) error {
	if clicore.FirstPositional(args) != "migrations" {
		return datacli.DB(params, args, workspaceRoot, env, ioctx)
	}
	rest := clicore.DropFirstPositional(args)
	switch verb := clicore.FirstPositional(rest); verb {
	case "publish":
		return publishMigration(params, clicore.DropFirstPositional(rest), workspaceRoot, env, ioctx)
	case "", "help":
		return entryHelp(params, ioctx, "db migrations", []map[string]string{
			{"command": "cloud db migrations publish [<project>]", "description": "publish a project's migration bundle through the Data owner API"},
		})
	default:
		return newError("unknown cloud db migrations command: "+verb+"; expected publish", ExitUsage)
	}
}

// routeEnv serves `cloud env`: status is the retired root `cloud status`;
// doctor and enable are unchanged.
func routeEnv(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx IO) error {
	switch clicore.FirstPositional(args) {
	case "status":
		return runtimecli.EnvStatus(params, clicore.DropFirstPositional(args), workspaceRoot, env, ioctx)
	default:
		return runtimecli.Env(params, args, workspaceRoot, env, ioctx, envDoctorPublish)
	}
}

// entryHelp prints the verbs of one root entry.
func entryHelp(params map[string]any, ioctx IO, entry string, commands []map[string]string) error {
	if clicore.StructuredOutput(params) {
		writeResult(map[string]any{"commands": commands}, params, ioctx, "")
		return nil
	}
	width := 0
	for _, c := range commands {
		width = max(width, len(c["command"]))
	}
	ioctx.Stdout("@putnami/cloud " + entry + " commands:")
	for _, c := range commands {
		ioctx.Stdout(fmt.Sprintf("  putnami %-*s  %s", width, c["command"], c["description"]))
	}
	return nil
}
