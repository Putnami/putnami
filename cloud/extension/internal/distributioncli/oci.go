package distributioncli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	ociserverclient "go.putnami.dev/cloud/clients/oci-server/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// Capability strings the Putnami OCI registry advertises on
// GET /v2/_putnami/capabilities. They mirror the server-side constants of the
// OCI registry (oci-server); the probe-and-fall-back protocol means an older
// registry that advertises neither keeps working through the standard
// distribution API.
const (
	ociCapabilityTagDigestV1 = "tag-digest/v1"
	ociCapabilityCopyV1      = "copy/v1"
	ociCapabilityRevertV1    = "revert/v1"
)

// ociSourceRefHeader carries the caller-supplied opaque provenance ref (a CI run
// id, a commit SHA) that the registry records on every channel/tag move these
// verbs perform. It mirrors channel.SourceRefHeader on the server; the constant
// is duplicated rather than imported so the CLI keeps no dependency on the
// registry's core module.
const ociSourceRefHeader = "X-Putnami-Source-Ref"

// ociManifestAcceptHeader is the Accept list sent when resolving a manifest,
// covering the OCI and Docker single-image and index media types.
const ociManifestAcceptHeader = "application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.v2+json, " +
	"application/vnd.oci.image.index.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json"

// ociRef is a registry-relative image reference: "ns/name", "ns/name:tag", or
// "ns/name@sha256:<64 hex>". The registry host is not part of the ref — these
// verbs always target the Putnami OCI registry resolved through the registry
// token seam (defaultRegistryURL[RegistryOCI], PUTNAMI_REGISTRY_OCI_URL, or a
// stored `cloud login` endpoint).
type ociRef struct {
	Repo   string
	Tag    string
	Digest string
}

// reference is the manifest reference used to resolve the ref: the digest when
// pinned, the tag when named, "latest" otherwise (the OCI client default).
func (r ociRef) reference() string {
	if r.Digest != "" {
		return r.Digest
	}
	if r.Tag != "" {
		return r.Tag
	}
	return "latest"
}

func (r ociRef) String() string {
	if r.Digest != "" {
		return r.Repo + "@" + r.Digest
	}
	if r.Tag != "" {
		return r.Repo + ":" + r.Tag
	}
	return r.Repo
}

// OCICopyResult is the reference the copy command reports.
type OCICopyResult struct {
	Source      string   `json:"source"`
	Digest      string   `json:"digest"`
	Destination string   `json:"destination"`
	Tags        []string `json:"tags,omitempty"`
	// FastPath is true when the server-side copy endpoint handled the copy
	// (zero blob bytes moved), false when the standard-API fallback ran.
	FastPath bool `json:"fast_path"`
}

// OCIRetagResult is the reference the retag command reports.
type OCIRetagResult struct {
	Repository string   `json:"repository"`
	Digest     string   `json:"digest"`
	Tags       []string `json:"tags"`
	FastPath   bool     `json:"fast_path"`
}

// OCIRevertResult is the restored reference the revert command reports.
type OCIRevertResult struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	// Digest is the manifest the tag was restored to — the target recorded
	// before the move being undone.
	Digest string `json:"digest"`
	// Reverted is the manifest the tag carried before the revert.
	Reverted string `json:"reverted"`
}

// OCI backs `putnami cloud packages copy|retag|revert` (alias: `cloud oci`): server-side image copy,
// retag, and tag rollback against the Putnami OCI registry.
//
//	putnami cloud packages copy <src-ref> <dst-ref>
//	putnami cloud packages retag <ref> <tag...>
//	putnami cloud packages revert <repo>:<tag>
//
// copy and retag probe GET /v2/_putnami/capabilities once and use the advertised
// fast path (POST /v2/_putnami/copy / /v2/_putnami/tag-digest); a registry that
// does not advertise the capability is driven through the standard OCI
// distribution API instead. revert has no such fallback: restoring a tag's prior
// target requires the registry's recorded move history, which the standard API
// does not expose. Credentials come exclusively from the registry token seam
// (`cloud login` / `cloud registries setup`); provider credentials are never
// consulted.
//
// Every verb accepts --source-ref: an opaque provenance string (a CI run id, a
// commit SHA) the registry records verbatim on each move it performs.
func OCI(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	pos := commandPositionals(args)
	sub := ""
	if len(pos) > 0 {
		sub = pos[0]
	}
	switch sub {
	case "copy":
		return ociRunCopy(params, pos[1:], env, ioctx)
	case "retag":
		return ociRunRetag(params, pos[1:], env, ioctx)
	case "revert":
		return ociRunRevert(params, pos[1:], env, ioctx)
	default:
		return clicore.NewError(
			"unknown cloud packages command: "+sub+`; expected "copy <src-ref> <dst-ref>", "retag <ref> <tag...>", or "revert <repo>:<tag>"`,
			clicore.ExitUsage)
	}
}

// ociSourceRef reads the caller-supplied provenance ref from --source-ref.
func ociSourceRef(params map[string]any) string {
	return strings.TrimSpace(clicore.StringParam(params, "source-ref", "sourceRef"))
}

func ociRunCopy(params map[string]any, rest []string, env map[string]string, ioctx clicore.IO) error {
	if len(rest) != 2 {
		return clicore.NewError(
			"cloud packages copy requires <src-ref> <dst-ref> (e.g. putnami cloud packages copy ns/app:latest ns/app-mirror:latest)",
			clicore.ExitUsage)
	}
	src, err := parseOCIRef(rest[0])
	if err != nil {
		return err
	}
	dst, err := parseOCIRef(rest[1])
	if err != nil {
		return err
	}
	if dst.Digest != "" {
		return clicore.NewError("cloud packages copy destination must be <repo>[:tag] — the copied manifest keeps its digest", clicore.ExitUsage)
	}

	baseURL, token, err := resolveOCICredentials(params, env, ioctx)
	if err != nil {
		return err
	}
	res, err := ociCopyImage(commandContext(ioctx), ioctx.Client, baseURL, token, src, dst, ociSourceRef(params))
	if err != nil {
		return err
	}

	how := "server-side (metadata only)"
	if !res.FastPath {
		how = "via the standard distribution API"
	}
	clicore.WriteResult(res, params, ioctx,
		fmt.Sprintf("Copied %s@%s to %s %s on %s.", res.Source, clicore.ShortDigest(res.Digest), res.Destination, how, baseURL))
	return nil
}

func ociRunRetag(params map[string]any, rest []string, env map[string]string, ioctx clicore.IO) error {
	if len(rest) < 2 {
		return clicore.NewError(
			"cloud packages retag requires <ref> <tag...> (e.g. putnami cloud packages retag ns/app@sha256:... stable v2)",
			clicore.ExitUsage)
	}
	ref, err := parseOCIRef(rest[0])
	if err != nil {
		return err
	}
	tags := rest[1:]

	baseURL, token, err := resolveOCICredentials(params, env, ioctx)
	if err != nil {
		return err
	}
	res, err := ociRetagImage(commandContext(ioctx), ioctx.Client, baseURL, token, ref, tags, ociSourceRef(params))
	if err != nil {
		return err
	}
	clicore.WriteResult(res, params, ioctx,
		fmt.Sprintf("Tagged %s@%s as [%s] on %s.", res.Repository, clicore.ShortDigest(res.Digest), strings.Join(res.Tags, ", "), baseURL))
	return nil
}

// ociRunRevert backs `putnami cloud packages revert <repo>:<tag>`: restore a tag to
// the target recorded before its last move.
func ociRunRevert(params map[string]any, rest []string, env map[string]string, ioctx clicore.IO) error {
	if len(rest) != 1 {
		return clicore.NewError(
			"cloud packages revert requires <repo>:<tag> (e.g. putnami cloud packages revert ns/app:stable)",
			clicore.ExitUsage)
	}
	ref, err := parseOCIRef(rest[0])
	if err != nil {
		return err
	}
	if ref.Digest != "" {
		return clicore.NewError("cloud packages revert takes <repo>:<tag> — a digest reference is immutable and cannot be reverted", clicore.ExitUsage)
	}
	if ref.Tag == "" {
		return clicore.NewError("cloud packages revert requires an explicit tag: <repo>:<tag>", clicore.ExitUsage)
	}

	baseURL, token, err := resolveOCICredentials(params, env, ioctx)
	if err != nil {
		return err
	}
	res, err := ociRevertTag(commandContext(ioctx), ioctx.Client, baseURL, token, ref, ociSourceRef(params))
	if err != nil {
		return err
	}
	clicore.WriteResult(res, params, ioctx,
		fmt.Sprintf("Reverted %s:%s from %s to %s on %s.",
			res.Repository, res.Tag, clicore.ShortDigest(res.Reverted), clicore.ShortDigest(res.Digest), baseURL))
	return nil
}

// ociRevertTag restores a tag to the target the registry recorded before its
// last move. There is no standard-distribution-API fallback: the prior target
// lives in the registry's append-only move log, so a registry that does not
// advertise revert/v1 simply cannot do this — say so rather than pretend.
func ociRevertTag(ctx context.Context, client *http.Client, baseURL, token string, ref ociRef, sourceRef string) (OCIRevertResult, error) {
	if !ociServerAdvertises(ctx, client, baseURL, ociCapabilityRevertV1) {
		return OCIRevertResult{}, clicore.NewError(
			"this registry does not advertise revert/v1: reverting a tag needs the registry's recorded "+
				"channel-move history, which the standard distribution API does not expose — "+
				"retag the intended digest explicitly instead",
			clicore.ExitAPI)
	}
	body := map[string]any{"repository": ref.Repo, "tag": ref.Tag}
	raw, err := postOCIFastPathJSON(ctx, client, baseURL, token, "revert", body, sourceRef)
	if err != nil {
		return OCIRevertResult{}, err
	}
	var decoded OCIRevertResult
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return OCIRevertResult{}, clicore.NewError("cloud packages revert: invalid response body: "+err.Error(), clicore.ExitAPI)
	}
	if decoded.Repository == "" {
		decoded.Repository = ref.Repo
	}
	if decoded.Tag == "" {
		decoded.Tag = ref.Tag
	}
	return decoded, nil
}

// ociCopyImage copies src to dst within one Putnami OCI registry. When the
// registry advertises copy/v1 the whole copy is one server-side call. The
// fallback drives the standard distribution API against the same registry: a
// cross-repo blob mount per referenced blob (still zero byte movement inside
// one registry) plus a manifest PUT. When the registry refuses a mount, the
// blob is pulled from the source repo and re-pushed monolithically — the only
// leg that moves bytes, kept as a last resort. Copying between two different
// registries is out of scope: both refs resolve against the single configured
// Putnami OCI endpoint.
func ociCopyImage(ctx context.Context, client *http.Client, baseURL, token string, src, dst ociRef, sourceRef string) (OCICopyResult, error) {
	digest := src.Digest
	var payload []byte
	var mediaType string
	var err error
	if digest == "" {
		digest, mediaType, payload, err = fetchOCIManifest(ctx, client, baseURL, token, src.Repo, src.reference())
		if err != nil {
			return OCICopyResult{}, err
		}
	}
	tags := []string{}
	if dst.Tag != "" {
		tags = append(tags, dst.Tag)
	}
	result := OCICopyResult{Source: src.Repo, Digest: digest, Destination: dst.Repo, Tags: tags}

	if ociServerAdvertises(ctx, client, baseURL, ociCapabilityCopyV1) {
		body := map[string]any{
			"source":      src.Repo,
			"digest":      digest,
			"destination": dst.Repo,
			"tags":        tags,
		}
		if err := postOCIFastPath(ctx, client, baseURL, token, "copy", body, sourceRef); err != nil {
			return OCICopyResult{}, err
		}
		result.FastPath = true
		return result, nil
	}

	// Fallback: standard OCI distribution API against the same registry.
	if payload == nil {
		_, mediaType, payload, err = fetchOCIManifest(ctx, client, baseURL, token, src.Repo, digest)
		if err != nil {
			return OCICopyResult{}, err
		}
	}
	// The fallback mounts config+layer blobs and re-PUTs one manifest; it does
	// not chase an index's child manifests, so it would produce a destination
	// that resolves the index digest but 404s every platform. Refuse it up front
	// rather than write a broken copy — the copy/v1 fast path (which the Putnami
	// registry advertises) handles multi-arch; against a registry that does not,
	// copy each platform manifest by digest.
	if ociManifestIsIndex(payload) {
		return OCICopyResult{}, clicore.NewError(
			"copying a multi-arch image index requires a registry that advertises copy/v1; "+
				"this registry offers only the standard distribution API, which cannot copy an "+
				"index's child manifests — copy each platform manifest by digest instead",
			clicore.ExitAPI)
	}
	for _, d := range ociManifestBlobDigests(payload) {
		if err := mountOrTransferOCIBlob(ctx, client, baseURL, token, src.Repo, dst.Repo, d); err != nil {
			return OCICopyResult{}, err
		}
	}
	// The manifest PUT creates the destination version; a tagged destination
	// also moves the tag, a bare one is addressed by digest only.
	reference := dst.Tag
	if reference == "" {
		reference = digest
	}
	if err := putOCIManifest(ctx, client, baseURL, token, dst.Repo, reference, mediaType, payload, sourceRef); err != nil {
		return OCICopyResult{}, err
	}
	return result, nil
}

// ociRetagImage assigns tags to the digest ref resolves to. With the
// tag-digest/v1 fast path all tags land in one digest-addressed call; the
// fallback re-PUTs the manifest bytes under each tag, which converges to the
// same end state.
func ociRetagImage(ctx context.Context, client *http.Client, baseURL, token string, ref ociRef, tags []string, sourceRef string) (OCIRetagResult, error) {
	digest := ref.Digest
	var payload []byte
	var mediaType string
	var err error
	if digest == "" {
		digest, mediaType, payload, err = fetchOCIManifest(ctx, client, baseURL, token, ref.Repo, ref.reference())
		if err != nil {
			return OCIRetagResult{}, err
		}
	}
	result := OCIRetagResult{Repository: ref.Repo, Digest: digest, Tags: tags}

	if ociServerAdvertises(ctx, client, baseURL, ociCapabilityTagDigestV1) {
		body := map[string]any{
			"repository": ref.Repo,
			"digest":     digest,
			"tags":       tags,
		}
		if err := postOCIFastPath(ctx, client, baseURL, token, "tag-digest", body, sourceRef); err != nil {
			return OCIRetagResult{}, err
		}
		result.FastPath = true
		return result, nil
	}

	if payload == nil {
		_, mediaType, payload, err = fetchOCIManifest(ctx, client, baseURL, token, ref.Repo, digest)
		if err != nil {
			return OCIRetagResult{}, err
		}
	}
	for _, tag := range tags {
		if err := putOCIManifest(ctx, client, baseURL, token, ref.Repo, tag, mediaType, payload, sourceRef); err != nil {
			return OCIRetagResult{}, err
		}
	}
	return result, nil
}

// resolveOCICredentials returns the Putnami OCI registry base URL and bearer
// through the EXISTING registry token seam. A stored short-lived Distribution
// lease JWT — e.g. one written into Docker config by `cloud token --for oci
// --owner-workspace <id> --package <pkg> --materialize` — is used directly as
// the bearer; only an already-persisted legacy opaque pkt_* api key still goes
// through the compatibility api-key exchange (like the retired
// `cloud token --for oci`). Provider credentials (docker config for other
// registries, cloud provider auth) are never used. The endpoint defaults to
// defaultRegistryURL[RegistryOCI] (https://oci.putnami.dev) and honors
// PUTNAMI_REGISTRY_OCI_URL / --registry-oci-url via resolveRegistryEndpoints.
func resolveOCICredentials(params map[string]any, env map[string]string, ioctx clicore.IO) (baseURL, token string, err error) {
	resolved, err := resolveRegistryTokenForRegistry(params, env, RegistryOCI)
	if err != nil {
		return "", "", err
	}
	token = resolved.Token
	if !isDistributionLeaseBearer(token) {
		token, err = exchangeRegistryTokenJWT(params, env, ioctx, resolved.Endpoint, resolved.Token)
		if err != nil {
			return "", "", err
		}
	}
	baseURL = resolved.Endpoint.URL
	if baseURL == "" {
		baseURL = defaultRegistryURL[RegistryOCI]
	}
	return baseURL, token, nil
}

// ociServerAdvertises probes the unauthenticated capabilities route through
// oci-server's generated client. Any failure — a transport error, a refusal, an
// older registry without the route, an answer outside the provider contract —
// means baseline-only, and the caller falls back to the standard distribution
// API. The probe stays credential-free: the binding declares no credential, so
// this request cannot carry the registry bearer the fast-path calls it gates
// use.
func ociServerAdvertises(ctx context.Context, httpClient *http.Client, baseURL, capability string) bool {
	probe, err := ociCapabilitiesClient(httpClient, baseURL)
	if err != nil {
		return false
	}
	answer, err := probe.ListV2PutnamiCapabilities(ctx, ociserverclient.ListV2PutnamiCapabilitiesInput{})
	if err != nil || answer == nil {
		return false
	}
	return slices.Contains(clicore.Deref(answer.Apis), capability)
}

// postOCIFastPath posts a JSON body to /v2/_putnami/<api>. Unlike the probe,
// a fast-path rejection is surfaced as an error rather than silently retried
// through the fallback: the registry advertised the capability, so a 4xx here
// is a real validation/authorization verdict that the fallback would only
// repeat with more round trips.
func postOCIFastPath(ctx context.Context, client *http.Client, baseURL, token, api string, body map[string]any, sourceRef string) error {
	_, err := postOCIFastPathJSON(ctx, client, baseURL, token, api, body, sourceRef)
	return err
}

// postOCIFastPathJSON is postOCIFastPath returning the response body, for the
// verbs whose result the registry computes (revert resolves the restored digest
// server-side from its move history).
func postOCIFastPathJSON(ctx context.Context, client *http.Client, baseURL, token, api string, body map[string]any, sourceRef string) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, clicore.NewError("marshal "+api+" request: "+err.Error(), clicore.ExitAPI)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(baseURL, "/")+"/v2/_putnami/"+api, bytes.NewReader(data))
	if err != nil {
		return nil, clicore.NewError("build "+api+" request: "+err.Error(), clicore.ExitAPI)
	}
	setBearer(req, token)
	req.Header.Set("Content-Type", "application/json")
	setSourceRef(req, sourceRef)
	resp, respBody, err := doJSON(client, req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, clicore.NewError(fmt.Sprintf("oci %s: %s: %s", api, resp.Status, string(respBody)), clicore.ExitAPI)
	}
	return respBody, nil
}

// setSourceRef attaches the caller-supplied provenance ref when one was given.
// An absent ref is not an error: the registry still records the move, just
// without an origin.
func setSourceRef(req *http.Request, sourceRef string) {
	if sourceRef != "" {
		req.Header.Set(ociSourceRefHeader, sourceRef)
	}
}

// fetchOCIManifest resolves a manifest reference (tag or digest) and returns
// its content digest, media type, and raw payload bytes (never re-encoded, so
// the digest stays valid).
func fetchOCIManifest(ctx context.Context, client *http.Client, baseURL, token, repo, reference string) (digest, mediaType string, payload []byte, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(baseURL, "/")+"/v2/"+repo+"/manifests/"+clicore.URLPathEscape(reference), nil)
	if err != nil {
		return "", "", nil, clicore.NewError("build manifest request: "+err.Error(), clicore.ExitAPI)
	}
	setBearer(req, token)
	req.Header.Set("Accept", ociManifestAcceptHeader)
	resp, body, err := doJSON(client, req)
	if err != nil {
		return "", "", nil, err
	}
	if resp.StatusCode >= 400 {
		return "", "", nil, clicore.NewError(
			fmt.Sprintf("resolve manifest %s/%s: %s: %s", repo, reference, resp.Status, string(body)), clicore.ExitAPI)
	}
	digest = resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		sum := sha256.Sum256(body)
		digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	mediaType = resp.Header.Get("Content-Type")
	if mediaType == "" {
		mediaType = "application/vnd.oci.image.manifest.v1+json"
	}
	return digest, mediaType, body, nil
}

// mountOrTransferOCIBlob makes digest readable through dstRepo. It first asks
// for a cross-repo mount (POST ?mount=&from=), which is metadata-only inside
// one registry. A 202 means the registry refused the mount and opened a
// regular upload session instead (the spec-prescribed fallback); only then are
// the bytes pulled from the source repo and re-pushed monolithically.
func mountOrTransferOCIBlob(ctx context.Context, client *http.Client, baseURL, token, srcRepo, dstRepo, digest string) error {
	mountURL := strings.TrimRight(baseURL, "/") + "/v2/" + dstRepo + "/blobs/uploads?" + url.Values{
		"mount": {digest},
		"from":  {srcRepo},
	}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mountURL, nil)
	if err != nil {
		return clicore.NewError("build blob mount request: "+err.Error(), clicore.ExitAPI)
	}
	setBearer(req, token)
	resp, body, err := doJSON(client, req)
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusCreated:
		return nil // mounted — no bytes moved
	case resp.StatusCode == http.StatusAccepted:
		// Mount refused; pull the blob through the source repo and push it.
	default:
		return clicore.NewError(fmt.Sprintf("mount blob %s into %s: %s: %s", digest, dstRepo, resp.Status, string(body)), clicore.ExitAPI)
	}

	getReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(baseURL, "/")+"/v2/"+srcRepo+"/blobs/"+clicore.URLPathEscape(digest), nil)
	if err != nil {
		return clicore.NewError("build blob request: "+err.Error(), clicore.ExitAPI)
	}
	setBearer(getReq, token)
	getResp, blobBody, err := doJSON(client, getReq)
	if err != nil {
		return err
	}
	if getResp.StatusCode >= 400 {
		return clicore.NewError(fmt.Sprintf("pull blob %s from %s: %s", digest, srcRepo, getResp.Status), clicore.ExitAPI)
	}

	putURL := strings.TrimRight(baseURL, "/") + "/v2/" + dstRepo + "/blobs/uploads?" + url.Values{
		"digest": {digest},
	}.Encode()
	putReq, err := http.NewRequestWithContext(ctx, http.MethodPost, putURL, bytes.NewReader(blobBody))
	if err != nil {
		return clicore.NewError("build blob upload request: "+err.Error(), clicore.ExitAPI)
	}
	setBearer(putReq, token)
	putReq.Header.Set("Content-Type", "application/octet-stream")
	putResp, putBody, err := doJSON(client, putReq)
	if err != nil {
		return err
	}
	if putResp.StatusCode >= 400 {
		return clicore.NewError(fmt.Sprintf("push blob %s to %s: %s: %s", digest, dstRepo, putResp.Status, string(putBody)), clicore.ExitAPI)
	}
	return nil
}

// putOCIManifest pushes the raw manifest payload under reference (tag or
// digest), preserving the source media type so the content digest holds.
func putOCIManifest(ctx context.Context, client *http.Client, baseURL, token, repo, reference, mediaType string, payload []byte, sourceRef string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		strings.TrimRight(baseURL, "/")+"/v2/"+repo+"/manifests/"+clicore.URLPathEscape(reference), bytes.NewReader(payload))
	if err != nil {
		return clicore.NewError("build manifest push request: "+err.Error(), clicore.ExitAPI)
	}
	setBearer(req, token)
	req.Header.Set("Content-Type", mediaType)
	setSourceRef(req, sourceRef)
	resp, body, err := doJSON(client, req)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return clicore.NewError(fmt.Sprintf("push manifest %s/%s: %s: %s", repo, reference, resp.Status, string(body)), clicore.ExitAPI)
	}
	return nil
}

// ociManifestBlobDigests extracts the content-addressed blobs a manifest
// references (config + layers). Child manifests of an image index are not
// blobs and are not returned; the fallback rejects an index up front (see
// ociManifestIsIndex) because it cannot chase those children, while the
// copy/v1 fast path copies the whole tree server-side.
func ociManifestBlobDigests(payload []byte) []string {
	var m struct {
		Config *struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil
	}
	out := make([]string, 0, len(m.Layers)+1)
	if m.Config != nil && m.Config.Digest != "" {
		out = append(out, m.Config.Digest)
	}
	for _, layer := range m.Layers {
		if layer.Digest != "" {
			out = append(out, layer.Digest)
		}
	}
	return out
}

// ociManifestIsIndex reports whether payload is an image index / manifest list
// — one that references child platform manifests rather than config + layers.
// The standard-API fallback cannot copy an index's children, so it refuses one.
func ociManifestIsIndex(payload []byte) bool {
	var m struct {
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(payload, &m); err != nil {
		return false
	}
	return len(m.Manifests) > 0
}

// parseOCIRef parses a registry-relative reference: "repo", "repo:tag", or
// "repo@sha256:<64 hex>" where repo is "ns/name" or "name".
func parseOCIRef(raw string) (ociRef, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ociRef{}, clicore.NewError("empty image reference", clicore.ExitUsage)
	}
	if repo, digest, ok := strings.Cut(raw, "@"); ok {
		if repo == "" || !isOCISHA256Digest(digest) {
			return ociRef{}, clicore.NewError("invalid image reference "+raw+" (want repo@sha256:<64 hex>)", clicore.ExitUsage)
		}
		return ociRef{Repo: repo, Digest: digest}, nil
	}
	if repo, tag, ok := strings.Cut(raw, ":"); ok {
		if repo == "" || tag == "" {
			return ociRef{}, clicore.NewError("invalid image reference "+raw+" (want repo:tag)", clicore.ExitUsage)
		}
		return ociRef{Repo: repo, Tag: tag}, nil
	}
	return ociRef{Repo: raw}, nil
}

func isOCISHA256Digest(d string) bool {
	rest, ok := strings.CutPrefix(d, "sha256:")
	if !ok || len(rest) != 64 {
		return false
	}
	for _, c := range rest {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}

// commandPositionals collects the non-flag arguments, skipping the parent
// CLI's --putnamiContext token and the values consumed by string flags (the
// same walk clicore.FirstPositional performs, kept local because the oci and
// distribution verbs take multiple positionals).
func commandPositionals(args []string) []string {
	out := []string{}
	skipNext := false
	for i, raw := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if raw == "--putnamiContext" {
			skipNext = true
			continue
		}
		if strings.HasPrefix(raw, "--") {
			name := strings.TrimPrefix(raw, "--")
			if strings.Contains(name, "=") || strings.HasPrefix(name, "no-") || clicore.IsBooleanFlag(name) {
				continue
			}
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				skipNext = true
			}
			continue
		}
		out = append(out, raw)
	}
	return out
}
