package cloudcli

import clicore "go.putnami.dev/cloud/extension/internal/clicore"

// Compatibility aliases for the shared CLI toolkit in internal/clicore.
// They let the aggregator's own code — the cross-cutting commands (login/logout/
// setup/install/token), the dispatch + context plumbing, and the white-box
// tests — keep referring to the toolkit by its original lowercase names instead
// of clicore.*. The per-domain command packages (internal/deliverycli,
// configcli, distributioncli, runtimecli, identitycli) call clicore.* directly and do NOT use this shim; it shrank
// as each was extracted, and shrinks further if the remaining aggregator call
// sites are converted to clicore.* (a mechanical follow-up).

// Constants.
const (
	DefaultAuthURL         = clicore.DefaultAuthURL
	DefaultControlPlaneURL = clicore.DefaultControlPlaneURL
	AuthFileRelative       = clicore.AuthFileRelative
	LinkFileRelative       = clicore.LinkFileRelative

	ExitSuccess = clicore.ExitSuccess
	ExitUsage   = clicore.ExitUsage
	ExitAuth    = clicore.ExitAuth
	ExitAPI     = clicore.ExitAPI

	userAgentEnvVar   = clicore.UserAgentEnvVar
	fallbackUserAgent = clicore.FallbackUserAgent
)

// Types.
type (
	IO       = clicore.IO
	cliError = clicore.ExitError

	storedToken           = clicore.StoredToken
	storedWorkspaceAccess = clicore.StoredWorkspaceAccess
)

// Functions and values.
var (
	newError = clicore.NewError

	sendJSON = clicore.SendJSON

	cliUserAgent  = clicore.CLIUserAgent
	setUserAgent  = clicore.SetUserAgent
	withUserAgent = clicore.WithUserAgent

	urlPathEscape = clicore.URLPathEscape
	envGet        = clicore.EnvGet

	param       = clicore.Param
	stringParam = clicore.StringParam
	truthy      = clicore.Truthy
	boolParam   = clicore.BoolParam

	valueString = clicore.ValueString
	stringValue = clicore.StringValue
	firstString = clicore.FirstString

	writeResult = clicore.WriteResult

	resolveApp           = clicore.ResolveApp
	findAppDir           = clicore.FindAppDir
	canonicalProjectName = clicore.CanonicalProjectName
	projectNameTail      = clicore.ProjectNameTail
	readCloudLink        = clicore.ReadCloudLink
	readManifestLink     = clicore.ReadManifestLink
	findActiveProject    = clicore.FindActiveProject
	linkPath             = clicore.LinkPath
	projectIDFromPath    = clicore.ProjectIDFromPath

	activeAuth                      = clicore.ActiveAuth
	workspaceAuth                   = clicore.WorkspaceAuth
	refreshStoredAuthWithRetry      = clicore.RefreshStoredAuthWithRetry
	refreshStoredAuthFreshWithRetry = clicore.RefreshStoredAuthFreshWithRetry
	newOAuthTokenError              = clicore.NewOAuthTokenError
	isOAuthTokenError               = clicore.IsOAuthTokenError
	authEndpoints                   = clicore.AuthEndpoints
	controlPlaneBaseURL             = clicore.ControlPlaneBaseURL
	readAuth                        = clicore.ReadAuth
	writeAuth                       = clicore.WriteAuth
	removeAuth                      = clicore.RemoveAuth
	decodeJWT                       = clicore.DecodeJWT
	claimedWorkspaceID              = clicore.ClaimedWorkspaceID

	parseFlags         = clicore.ParseFlags
	adoptPositionalApp = clicore.AdoptPositionalApp
	mergeParams        = clicore.MergeParams
	isBooleanFlag      = clicore.IsBooleanFlag
)

// init registers the cloud CLI's boolean (value-less) flags with clicore so its
// generic flag parser knows which flags stand alone. This is the cloud CLI's
// declared flag vocabulary — clicore hardcodes none of it.
func init() {
	clicore.RegisterBooleanFlags(
		"json", "open", "replace", "yes", "auto", "cache", "wait", "dry-run", "dryRun", "force",
		"include-secrets", "includeSecrets", "with-secrets", "withSecrets",
		"schema", "secret-keys", "secretKeys", "reveal-secrets", "revealSecrets",
		"from-stdin", "fromStdin", "reveal",
		"stdin", "confirm", "execute",
		"auto-iam-authn", "autoIamAuthn", "health-check", "healthCheck", "supervise",
		"for-registry", "forRegistry", "global", "opaque", "materialize", "include-revoked", "remove", "if-present", "ifPresent",
		"skip-publish-config", "skipPublishConfig",
		"skip-publish-migration", "skipPublishMigration", "stable", "archives",
		"skip-publish-doc", "skipPublishDoc",
		"skip-publish-deployment", "skipPublishDeployment",
		// config put --roll-shared opts a shared/overlay-block write into the
		// bounded inline-primary fleet roll.
		"roll-shared", "rollShared",
		// observability/logs: --all follows every page of the paged query; --follow
		// switches `putnami cloud logs` to the live SSE tail.
		"all", "follow",
		// status: --provenance switches the human table to the per-workload deploy
		// provenance chain (commit → release → revision → config version);
		// --health adds the errors, top error, and on-main columns.
		"provenance", "health",
		// env doctor: --strict fails on every row that is not ok. Declared
		// here so `--strict staging` never takes the environment as its value.
		"strict",
		// ci: --cancel-running cancels in-flight runs when pausing the pipeline.
		"cancel-running", "cancelRunning",
		// ci logs: --raw prints the runner's protocol records verbatim instead of the
		// human rendering.
		"raw",
		// image-layers: --check verifies a produced layer set against the
		// workspace pins instead of producing one. ci fmt --check
		// verifies putnami.ci.json is canonical without writing it.
		"check",
		// config show: --declared, --keys and --metadata select the view the
		// retired `config <project>`, `config list` and `config resolve` printed.
		"declared", "keys", "metadata",
	)
}
