package configcli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// driftCloudTokenEnv is an explicit cloud control-plane bearer override for the
// drift command. It is the SAME env the framework runtime consumes as the cloud
// bearer (the cloudconfig token source: PUTNAMI_CLOUD_TOKEN). It is for a human
// JWT or an out-of-band service-account token supplied for local runs — NOT a
// workspace machine token (pkt_*), which 403s on config routes by design. When
// unset and no interactive session exists, drift authenticates as the runner's
// Cloud Run service account via a GCP metadata ID token.
const driftCloudTokenEnv = "PUTNAMI_CLOUD_TOKEN"

// driftConfigAudienceEnv overrides the audience the drift command mints its
// service-account ID token for. Defaults to the cloud link's control_plane_url
// (the config-server origin), which the CPA's GCP-ID-token middleware accepts via
// its AudienceAllowlist.
const driftConfigAudienceEnv = "PUTNAMI_CONFIG_SERVER_AUDIENCE"

// drift kinds classify one leaf-level divergence between the committed
// conf/env*.yaml projection and the published config plane.
const (
	driftCommittedNotPublished = "committed-not-published"
	driftPublishedNotCommitted = "published-not-committed"
	driftValueMismatch         = "value-mismatch"
)

// DriftEntry is one leaf-level divergence for structured output. Committed and
// Published hold schema-projected non-secret values, never secret plaintext.
type DriftEntry struct {
	Block     string `json:"block"`
	Key       string `json:"key"`
	Kind      string `json:"kind"`
	Committed any    `json:"committed,omitempty"`
	Published any    `json:"published,omitempty"`
}

// DriftEvidence is the credential-free comparison of the committed config with
// the published config plane that `cloud config drift` reports.
type DriftEvidence struct {
	CommittedFingerprint string       `json:"committed_fingerprint,omitempty"`
	PublishedFingerprint string       `json:"published_fingerprint,omitempty"`
	Drift                bool         `json:"drift,omitempty"`
	Entries              []DriftEntry `json:"entries,omitempty"`
}

// suppressedEntry is one divergence a managed layer legitimately owns, so it is
// NOT drift. It is reported (never silently dropped) so the CI log — the only
// audit trail a runner leaves — names every key the plane owns, the full layer
// dimension that owns it, and why it was exempted.
type suppressedEntry struct {
	Block     string `json:"block"`
	Key       string `json:"key"`
	Kind      string `json:"kind"`
	Layer     string `json:"layer"`
	Reason    string `json:"reason"`
	Committed any    `json:"committed,omitempty"`
	Published any    `json:"published,omitempty"`
}

// driftFingerprints identify the comparable, schema-projected portions of the
// committed source and published config plane. They deliberately exclude
// secrets, unknown keys, and active managed-overlay-owned differences — the
// same scope config drift uses to decide whether a deploy is safe to submit.
//
// These are config-plane fingerprints, not runtime CONFIG_DATA fingerprints.
// The release receipt separately records the latter because it also includes
// source-layer and secret-metadata inputs used by the control plane renderer.
type driftFingerprints struct {
	Committed string `json:"committed"`
	Published string `json:"published"`
}

// driftFingerprintLeaf preserves absent-versus-null semantics in the hashed
// representation. Both situations matter to config drift: an omitted leaf and
// a configured null are not interchangeable when a schema/publisher resolves
// them differently.
type driftFingerprintLeaf struct {
	Present bool `json:"present"`
	Value   any  `json:"value"`
}

// suppression reasons, one per ownership rule, rendered verbatim in both the
// human report and the structured output.
const (
	driftReasonUnset       = "absent from committed config; provisioned by the managed overlay"
	driftReasonPlaceholder = "committed fail-closed placeholder replaced by the provisioned binding"
)

// configRunDrift backs `putnami cloud config drift <project>`: a bidirectional
// git<->config-plane drift check. It projects the committed
// conf/env*.yaml through the SAME schema projection publish uses (planBlockWrites
// / publishValuesForSchema), resolves the published plane, and diffs the two per
// schema-declared, non-secret leaf — in BOTH directions. It exits 0 (quiet) when
// they agree and non-zero with a per-block, per-direction report when they drift.
func configRunDrift(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error { //nolint:unparam // command handler signature
	inspection, err := inspectConfigDrift(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	ctx := inspection.ctx
	entries := inspection.entries
	suppressed := inspection.suppressed

	out := map[string]any{
		"workspace":             ctx.workspaceID,
		"app":                   ctx.app,
		"environment":           ctx.environment,
		"schemaSource":          inspection.schemaSource,
		"valuesPaths":           inspection.valuesPaths,
		"committed_fingerprint": inspection.evidence.CommittedFingerprint,
		"published_fingerprint": inspection.evidence.PublishedFingerprint,
		"drift":                 len(entries) > 0,
		"entries":               entries,
		"suppressed":            suppressed,
		"fingerprints": driftFingerprints{
			Committed: inspection.evidence.CommittedFingerprint,
			Published: inspection.evidence.PublishedFingerprint,
		},
	}
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(out, params, ioctx, "")
		return driftError(ctx, entries)
	}
	// The suppressed detail prints in BOTH the clean and the drifted case: CI
	// keeps only stdout/stderr, so this is the audit trail for every key the
	// check chose not to report.
	if len(entries) == 0 {
		ioctx.Stdout(driftCleanLine(ctx, len(inspection.blocks), len(suppressed)))
		for _, line := range driftSuppressedLines(suppressed) {
			ioctx.Stdout(line)
		}
		return nil
	}
	for _, line := range driftReportLines(ctx, entries, suppressed) {
		ioctx.Stdout(line)
	}
	return driftError(ctx, entries)
}

type configDriftInspection struct {
	ctx          *secretsCtx
	blocks       []schemaBlock
	schemaSource string
	valuesPaths  []string
	entries      []DriftEntry
	suppressed   []suppressedEntry
	evidence     DriftEvidence
}

func inspectConfigDrift(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) (*configDriftInspection, error) {
	ctx, err := newConfigDriftCtx(params, workspaceRoot, env, ioctx)
	if err != nil {
		return nil, err
	}
	blocks, schemaSource, err := driftSchemaBlocks(params, ctx, workspaceRoot)
	if err != nil {
		return nil, err
	}

	// Committed side: the exact values publish would send, projected through the
	// schema (secrets and unknown keys stripped, defaults filled).
	valuesPaths := resolveValuesPaths(workspaceRoot, ctx.app, ctx.environment)
	committed, err := loadAndMergeYAML(valuesPaths)
	if err != nil {
		return nil, err
	}

	// Published side: the resolved plane. A default resolve carries no secrets;
	// the same schema projection then scopes the comparison to declared,
	// non-secret leaves so managed-overlay bindings the overlay injects under
	// non-schema keys never read as published-but-uncommitted false positives.
	resp, err := configResolveWithCtx(ctx, params, ioctx)
	if err != nil {
		return nil, err
	}

	managedPaths := managedOverlayPaths(resolveLayerDimensions(resp.Layers))
	entries, suppressed := computeConfigDrift(blocks, committed, resp.Config, managedPaths)
	fingerprints, err := configDriftFingerprints(blocks, committed, resp.Config, managedPaths)
	if err != nil {
		return nil, fmt.Errorf("fingerprint config drift inputs: %w", err)
	}
	evidence := DriftEvidence{
		CommittedFingerprint: fingerprints.Committed,
		PublishedFingerprint: fingerprints.Published,
		Drift:                len(entries) > 0,
		Entries:              append(make([]DriftEntry, 0, len(entries)), entries...),
	}
	return &configDriftInspection{
		ctx:          ctx,
		blocks:       blocks,
		schemaSource: schemaSource,
		valuesPaths:  valuesPaths,
		entries:      entries,
		suppressed:   suppressed,
		evidence:     evidence,
	}, nil
}

// configProjectionFingerprint hashes only schema-declared, non-secret leaves,
// using the same projection as publish/drift. It is stable across YAML
// formatting and map order and contains no secret plaintext.
func configProjectionFingerprint(blocks []schemaBlock, tree map[string]any) string {
	projected := map[string]any{}
	for _, block := range blocks {
		driftFlatten(block.Path, publishValuesForSchema(block.Fields, extractBlockValues(tree, block.Path)), projected)
	}
	body, err := json.Marshal(projected)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(body)
	return fmt.Sprintf("sha256:%x", digest[:])
}

// newConfigDriftCtx resolves the drift command's authenticated context in
// precedence order:
//
//  1. PUTNAMI_CLOUD_TOKEN — an explicit cloud bearer override (a human JWT or an
//     out-of-band SA token, for local runs).
//  2. an interactive session (`putnami cloud login`) — the human path.
//  3. otherwise (CI / non-interactive) a GCP metadata ID token minted as the
//     runner's Cloud Run service account, for the config-server audience.
//     A machine token (pkt_*) is deliberately NOT a fallback: it 403s on config
//     routes by design. A configured-but-unavailable metadata
//     source fails closed with a clear error — never an anonymous resolve.
//
// The {workspace, app, env, control-plane} discovery mirrors newSecretsCtx so
// every path resolves the same target.
func newConfigDriftCtx(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) (*secretsCtx, error) {
	if token := strings.TrimSpace(clicore.EnvGet(env, driftCloudTokenEnv)); token != "" {
		return driftCtxWithBearer(params, workspaceRoot, env, ioctx, clicore.NewBearer(token))
	}
	if driftHasSession(env) {
		ctx, err := newSecretsCtx(params, workspaceRoot, env, ioctx)
		if err != nil {
			return nil, err
		}
		ctx.controlPlane = clicore.ControlPlaneBaseURL(params, env, ctx.controlPlane)
		return ctx, nil
	}
	return newConfigDriftMetadataCtx(params, workspaceRoot, env, ioctx)
}

// driftHasSession reports whether a persisted interactive cloud session exists. A
// missing auth.json is the CI case, which falls through to metadata SA auth.
func driftHasSession(env map[string]string) bool {
	auth, err := clicore.ReadAuth(env, false)
	return err == nil && auth != nil && (auth.AccessToken != "" || auth.RefreshToken != "")
}

// newConfigDriftMetadataCtx authenticates the drift resolve as the runner's Cloud
// Run service account: it mints a GCP metadata ID token for the config-server
// audience and uses it as the resolve bearer. Fails closed (clear error) when the
// audience can't be resolved or the metadata source is unavailable.
func newConfigDriftMetadataCtx(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) (*secretsCtx, error) {
	link, err := clicore.ReadCloudLink(workspaceRoot)
	if err != nil {
		return nil, err
	}
	controlPlane := clicore.ControlPlaneBaseURL(params, env, clicore.StringValue(link["control_plane_url"]))
	audience := driftConfigAudience(params, env, controlPlane)
	if audience == "" {
		return nil, clicore.NewError(
			"config drift needs a config-server audience to mint a service-account token; set control_plane_url in the cloud link (run `putnami cloud setup`) or set "+driftConfigAudienceEnv,
			clicore.ExitUsage)
	}
	token, err := metadataIDToken(context.Background(), audience)
	if err != nil {
		return nil, clicore.NewError(
			fmt.Sprintf("config drift could not mint a service-account id token (aud=%q): %v", audience, err),
			clicore.ExitAuth)
	}
	if strings.TrimSpace(token) == "" {
		return nil, clicore.NewError(
			fmt.Sprintf("config drift minted an empty service-account id token (aud=%q)", audience),
			clicore.ExitAuth)
	}
	return driftCtxWithBearer(params, workspaceRoot, env, ioctx, clicore.NewBearer(strings.TrimSpace(token)))
}

// driftConfigAudience resolves the audience the SA id token is minted for: an
// explicit override (param/env) wins, else the config-server origin from the
// cloud link's control_plane_url.
func driftConfigAudience(params map[string]any, env map[string]string, controlPlane string) string {
	return clicore.TrimURL(clicore.FirstString(
		clicore.StringParam(params, "config-audience", "configAudience"),
		clicore.EnvGet(env, driftConfigAudienceEnv),
		controlPlane,
	))
}

// driftCtxWithBearer builds the authenticated drift context from a resolved
// bearer. The explicit CLI/environment control-plane origin takes precedence
// over the repository cloud link so a CI runner can pin where it sends the
// delegated bearer.
func driftCtxWithBearer(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO, bearer clicore.Bearer) (*secretsCtx, error) {
	link, err := clicore.ReadCloudLink(workspaceRoot)
	if err != nil {
		return nil, err
	}
	app, err := clicore.ResolveApp(params, workspaceRoot)
	if err != nil {
		return nil, err
	}
	environment := clicore.FirstString(clicore.StringParam(params, "env", "environment"), clicore.StringValue(link["environment"]), SecretsDefaultEnvironment)
	return &secretsCtx{
		workspaceID:  clicore.StringValue(link["workspace_id"]),
		controlPlane: clicore.ControlPlaneBaseURL(params, env, clicore.StringValue(link["control_plane_url"])),
		app:          app,
		environment:  environment,
		authToken:    bearer,
		io:           ioctx,
		params:       params,
		env:          env,
	}, nil
}

// driftSchemaBlocks resolves the schema blocks the drift compares against,
// preferring the LOCAL artifact (resolveSchemaPath) so drift reflects the schema
// the working tree would publish, and falling back to the PUBLISHED schema like
// configPutSchema does when no local artifact exists.
func driftSchemaBlocks(params map[string]any, ctx *secretsCtx, workspaceRoot string) ([]schemaBlock, string, error) {
	schemaPath, found, err := resolveSchemaPath(params, workspaceRoot, ctx.app)
	if err == nil && found {
		_, blocks, loadErr := loadSchemaManifest(schemaPath)
		return blocks, schemaPath, loadErr
	}
	// A bad --schema-from override is a hard failure, not a fall-through.
	if clicore.StringParam(params, "schema-from", "schemaFrom") != "" {
		return nil, "", err
	}
	schemaDoc, err := configFetchSchema(ctx)
	if err != nil {
		return nil, "", err
	}
	blocks, err := parseSchemaBlocks(schemaDoc, "published schema")
	if err != nil {
		return nil, "", err
	}
	return blocks, "published", nil
}

// computeConfigDrift diffs the committed values tree against the published plane
// per schema block, in both directions, over schema-declared non-secret leaves.
// Both sides go through the identical publishValuesForSchema projection publish
// uses, so the comparison is apples-to-apples (secrets/unknown keys stripped,
// declared defaults filled on both sides so a default never reads as drift).
// It returns the reportable drift entries AND the divergences an active managed
// layer legitimately owns, so the caller can print the suppressed set instead of
// dropping it silently.
func computeConfigDrift(blocks []schemaBlock, committed, published map[string]any, managedPaths map[string]managedOwnership) ([]DriftEntry, []suppressedEntry) {
	var entries []DriftEntry
	var suppressed []suppressedEntry
	for _, block := range blocks {
		committedLeaves := map[string]any{}
		publishedLeaves := map[string]any{}
		driftFlatten(block.Path, publishValuesForSchema(block.Fields, extractBlockValues(committed, block.Path)), committedLeaves)
		driftFlatten(block.Path, publishValuesForSchema(block.Fields, extractBlockValues(published, block.Path)), publishedLeaves)

		keys := unionKeys(committedLeaves, publishedLeaves)
		for _, key := range keys {
			cVal, cOK := committedLeaves[key]
			pVal, pOK := publishedLeaves[key]
			kind := driftKindFor(cVal, pVal, cOK, pOK)
			if kind == "" {
				continue
			}
			if owner, reason, ok := managedOverlaySuppresses(key, cVal, cOK, pOK, managedPaths); ok {
				suppressed = append(suppressed, suppressedEntry{
					Block: block.Path, Key: key, Kind: kind, Layer: owner.Layer, Reason: reason,
					Committed: cVal, Published: pVal,
				})
				continue
			}
			switch kind {
			case driftCommittedNotPublished:
				entries = append(entries, DriftEntry{Block: block.Path, Key: key, Kind: kind, Committed: cVal})
			case driftPublishedNotCommitted:
				entries = append(entries, DriftEntry{Block: block.Path, Key: key, Kind: kind, Published: pVal})
			case driftValueMismatch:
				entries = append(entries, DriftEntry{Block: block.Path, Key: key, Kind: kind, Committed: cVal, Published: pVal})
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Key != entries[j].Key {
			return entries[i].Key < entries[j].Key
		}
		return entries[i].Kind < entries[j].Kind
	})
	sort.Slice(suppressed, func(i, j int) bool {
		if suppressed[i].Key != suppressed[j].Key {
			return suppressed[i].Key < suppressed[j].Key
		}
		return suppressed[i].Kind < suppressed[j].Kind
	})
	return entries, suppressed
}

// configDriftFingerprints returns stable SHA-256 fingerprints for the two
// compared sides of config drift. It rebuilds the exact schema-projected leaf
// set used by computeConfigDrift, omitting only a divergence that the active
// managed overlay owns. That means a clean drift result always carries equal
// fingerprints, including when a release-pinned overlay has rewritten a
// config subtree; a reportable divergence necessarily changes one side.
func configDriftFingerprints(blocks []schemaBlock, committed, published map[string]any, managedPaths map[string]managedOwnership) (driftFingerprints, error) {
	committedLeaves := map[string]driftFingerprintLeaf{}
	publishedLeaves := map[string]driftFingerprintLeaf{}

	for _, block := range blocks {
		committedBlock := map[string]any{}
		publishedBlock := map[string]any{}
		driftFlatten(block.Path, publishValuesForSchema(block.Fields, extractBlockValues(committed, block.Path)), committedBlock)
		driftFlatten(block.Path, publishValuesForSchema(block.Fields, extractBlockValues(published, block.Path)), publishedBlock)

		for _, key := range unionKeys(committedBlock, publishedBlock) {
			committedValue, committedOK := committedBlock[key]
			publishedValue, publishedOK := publishedBlock[key]
			if kind := driftKindFor(committedValue, publishedValue, committedOK, publishedOK); kind != "" {
				if _, _, suppressed := managedOverlaySuppresses(key, committedValue, committedOK, publishedOK, managedPaths); suppressed {
					continue
				}
			}

			// A JSON numeric round-trip can change Go's concrete numeric type while
			// leaving the config value equal. Reuse the committed representation for
			// both hashes in that case so "no drift" always means equal fingerprints.
			if committedOK && publishedOK && driftValuesEqual(committedValue, publishedValue) {
				committedLeaves[key] = driftFingerprintLeaf{Present: true, Value: committedValue}
				publishedLeaves[key] = driftFingerprintLeaf{Present: true, Value: committedValue}
				continue
			}
			if committedOK {
				committedLeaves[key] = driftFingerprintLeaf{Present: true, Value: committedValue}
			} else {
				committedLeaves[key] = driftFingerprintLeaf{Present: false}
			}
			if publishedOK {
				publishedLeaves[key] = driftFingerprintLeaf{Present: true, Value: publishedValue}
			} else {
				publishedLeaves[key] = driftFingerprintLeaf{Present: false}
			}
		}
	}

	committedFingerprint, err := fingerprintDriftLeaves(committedLeaves)
	if err != nil {
		return driftFingerprints{}, err
	}
	publishedFingerprint, err := fingerprintDriftLeaves(publishedLeaves)
	if err != nil {
		return driftFingerprints{}, err
	}
	return driftFingerprints{Committed: committedFingerprint, Published: publishedFingerprint}, nil
}

func fingerprintDriftLeaves(leaves map[string]driftFingerprintLeaf) (string, error) {
	canonical, err := json.Marshal(leaves)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// driftKindFor classifies one leaf, or "" when the two sides agree.
func driftKindFor(committed, published any, committedOK, publishedOK bool) string {
	switch {
	case committedOK && !publishedOK:
		return driftCommittedNotPublished
	case !committedOK && publishedOK:
		return driftPublishedNotCommitted
	case committedOK && publishedOK && !driftValuesEqual(committed, published):
		return driftValueMismatch
	default:
		return ""
	}
}

// managedOverlayOwnedPaths is the explicit write contract of each managed
// overlay, keyed by its `managed/<provider>` dimension. No configbridge overlay
// emits a qualified `managed/<provider>/<qualifier…>` dimension. Database and
// storage merge these top-level document fields
// into their sections while preserving sibling operator configuration; events
// owns only the binding fields it injects. A listed object/map path owns its
// descendants. Ownership here is narrow on purpose: the additive overlay only
// fills keys the committed config leaves UNSET (configbridge
// EventsPushOverlay), so a committed non-placeholder value IS
// effective under this layer and any divergence on it is genuine drift.
// Keep this in sync with the control runtime's configbridge package.
var managedOverlayOwnedPaths = map[string][]string{
	"managed/database": {
		"database.$schema",
		"database.protocolVersion",
		"database.databases",
	},
	"managed/events": {
		"events.transport",
		"events.pubsub.projectId",
		"events.pubsub.topicTemplate",
		"events.push.issuer",
		"events.push.audience",
		"events.push.allowedServiceAccounts",
	},
	"managed/storage": {
		"storage.$schema",
		"storage.protocolVersion",
		"storage.bindings",
		"storage.providers.gcs.projectId",
	},
}

// managedOwnership records which managed layer owns a config path.
type managedOwnership struct {
	// Layer is the dimension as the plane reported it, so the audit trail
	// names the exact layer.
	Layer string
}

// managedOverlayPaths expands active managed resolution layers into the config
// paths those overlays own. A layer gets an exemption only when its exact
// dimension is declared above: an unknown managed layer, including any
// `managed/<provider>/<qualifier…>` dimension, fails closed. When two layers
// claim one path, the lower layer name wins, so the report is deterministic.
func managedOverlayPaths(layers []string) map[string]managedOwnership {
	owned := map[string]managedOwnership{}
	for _, layer := range layers {
		for _, path := range managedOverlayOwnedPaths[layer] {
			if current, seen := owned[path]; seen && current.Layer < layer {
				continue
			}
			owned[path] = managedOwnership{Layer: layer}
		}
	}
	return owned
}

// managedOverlaySuppresses reports whether an active managed layer owns this
// divergence, with the owning layer and the audit reason. A managed layer
// suppresses only what it can legitimately create: values absent from authored
// config, or empty fail-closed placeholders it replaces with a provisioned
// binding. The key must be one of that overlay's explicit owned paths (or a
// descendant), so a missing committed operator key beside a managed binding is
// still reported.
func managedOverlaySuppresses(key string, committed any, committedOK, publishedOK bool, managedPaths map[string]managedOwnership) (managedOwnership, string, bool) {
	owner, ok := managedOverlayOwnsPath(key, managedPaths)
	if !ok {
		return managedOwnership{}, "", false
	}
	reason, owns := managedOverlayOwnsDifference(committed, committedOK, publishedOK)
	if !owns {
		return managedOwnership{}, "", false
	}
	return owner, reason, true
}

// managedOverlayOwnsDifference is the additive rule: only a value absent from authored config, or an empty fail-closed placeholder the
// overlay replaces, is the overlay's to own.
func managedOverlayOwnsDifference(committed any, committedOK, publishedOK bool) (string, bool) {
	if !publishedOK {
		return "", false
	}
	if !committedOK {
		return driftReasonUnset, true
	}
	if isManagedPlaceholder(committed) {
		return driftReasonPlaceholder, true
	}
	return "", false
}

// managedOverlayOwnsPath resolves the most specific owned path covering key.
func managedOverlayOwnsPath(key string, managedPaths map[string]managedOwnership) (managedOwnership, bool) {
	best := ""
	owner := managedOwnership{}
	for path, candidate := range managedPaths {
		if key != path && !strings.HasPrefix(key, path+".") {
			continue
		}
		if best == "" || len(path) > len(best) || (len(path) == len(best) && path < best) {
			best, owner = path, candidate
		}
	}
	return owner, best != ""
}

func isManagedPlaceholder(value any) bool {
	if value == nil {
		return true
	}
	if text, ok := value.(string); ok {
		return text == ""
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice:
		return rv.Len() == 0
	default:
		return false
	}
}

// driftFlatten walks a projected block value into dotted leaf keys. Maps recurse
// (an empty map contributes nothing — it carries no config); every non-map value
// is a leaf, including slices, which are compared whole.
func driftFlatten(prefix string, value any, out map[string]any) {
	if m, ok := value.(map[string]any); ok {
		for k, sub := range m {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			driftFlatten(key, sub, out)
		}
		return
	}
	out[prefix] = value
}

func unionKeys(a, b map[string]any) []string {
	seen := map[string]bool{}
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// driftValuesEqual compares two projected leaf values by their canonical JSON
// encoding, so YAML/JSON numeric round-trips (int64 default vs float64 resolved)
// do not read as spurious drift.
func driftValuesEqual(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return reflect.DeepEqual(a, b)
	}
	return bytes.Equal(ab, bb)
}

// driftReportLines renders the human, per-block/per-direction drift report,
// followed by the plane-owned keys the check suppressed.
func driftReportLines(ctx *secretsCtx, entries []DriftEntry, suppressed []suppressedEntry) []string {
	lines := []string{fmt.Sprintf("Config drift for %s/%s (%d difference(s)):", ctx.app, ctx.environment, len(entries))}
	order, byBlock := groupByBlock(entries, func(e DriftEntry) string { return e.Block })
	for _, block := range order {
		lines = append(lines, "  block "+block+":")
		for _, e := range byBlock[block] {
			switch e.Kind {
			case driftCommittedNotPublished:
				lines = append(lines, fmt.Sprintf("    - %s committed but NOT published (%s)", e.Key, driftScalar(e.Committed)))
			case driftPublishedNotCommitted:
				lines = append(lines, fmt.Sprintf("    - %s published but NOT committed (%s)", e.Key, driftScalar(e.Published)))
			case driftValueMismatch:
				lines = append(lines, fmt.Sprintf("    - %s differs: committed=%s published=%s", e.Key, driftScalar(e.Committed), driftScalar(e.Published)))
			}
		}
	}
	lines = append(lines, fmt.Sprintf("Reconcile with `putnami cloud config publish %s --env %s` (committed→plane) or by committing the plane's values.", ctx.app, ctx.environment))
	return append(lines, driftSuppressedLines(suppressed)...)
}

// driftCleanLine is the quiet summary, carrying the suppressed count so a clean
// run still states how many keys the managed plane owns.
func driftCleanLine(ctx *secretsCtx, blocks, suppressed int) string {
	if suppressed == 0 {
		return fmt.Sprintf("No config drift for %s/%s (%d block(s) checked).", ctx.app, ctx.environment, blocks)
	}
	return fmt.Sprintf("No config drift for %s/%s (%d block(s) checked, %d plane-owned key(s) suppressed).",
		ctx.app, ctx.environment, blocks, suppressed)
}

// driftSuppressedLines renders the audit section: every key an active managed
// layer owns, the layer dimension, and why. These keys must NOT be reconciled
// with config publish: the overlay provisions them, so a pushed value would
// replace the provisioned binding with a stale copy.
func driftSuppressedLines(suppressed []suppressedEntry) []string {
	if len(suppressed) == 0 {
		return nil
	}
	lines := []string{fmt.Sprintf("Plane-owned key(s) suppressed (%d) — owned by an active managed layer, do NOT reconcile these with `config publish`:", len(suppressed))}
	order, byBlock := groupByBlock(suppressed, func(e suppressedEntry) string { return e.Block })
	for _, block := range order {
		lines = append(lines, "  block "+block+":")
		for _, e := range byBlock[block] {
			lines = append(lines, fmt.Sprintf("    - %s [%s] owned by layer %s: %s", e.Key, e.Kind, e.Layer, e.Reason))
		}
	}
	return lines
}

// groupByBlock buckets report rows by schema block in sorted block order,
// preserving each bucket's incoming (key, kind) order.
func groupByBlock[T any](items []T, block func(T) string) ([]string, map[string][]T) {
	byBlock := map[string][]T{}
	order := []string{}
	for _, item := range items {
		name := block(item)
		if _, ok := byBlock[name]; !ok {
			order = append(order, name)
		}
		byBlock[name] = append(byBlock[name], item)
	}
	sort.Strings(order)
	return order, byBlock
}

func driftScalar(value any) string {
	switch v := value.(type) {
	case nil:
		return "<absent>"
	case string:
		return v
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(data)
	}
}

// driftError is the non-zero exit when drift exists. It mirrors
// requireCompleteError's ExitAPI so the CI gate treats an actionable drift the
// same as a never-published block.
func driftError(ctx *secretsCtx, entries []DriftEntry) error {
	if len(entries) == 0 {
		return nil
	}
	return clicore.NewError(
		fmt.Sprintf("%d config drift(s) between committed values and the published plane for %s/%s", len(entries), ctx.app, ctx.environment),
		clicore.ExitAPI,
	)
}
