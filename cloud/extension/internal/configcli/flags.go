package configcli

import clicore "go.putnami.dev/cloud/extension/internal/clicore"

// init registers the config-domain's boolean (value-less) flags with clicore so
// its generic flag parser knows which flags stand alone. The aggregator
// registers the full cloud CLI vocabulary too; registration is additive and
// idempotent, so declaring this domain's own flags keeps the package self-contained
// (its positional parsing and tests don't depend on the aggregator running
// first).
func init() {
	clicore.RegisterBooleanFlags(
		"json", "dry-run", "dryRun",
		"include-secrets", "includeSecrets", "with-secrets", "withSecrets",
		"schema", "secret-keys", "secretKeys", "reveal-secrets", "revealSecrets",
		"from-stdin", "fromStdin", "reveal", "yes",
		"if-present", "ifPresent",
		"roll-shared", "rollShared",
		"require-complete", "requireComplete",
		"strict", "declared",
	)
}
