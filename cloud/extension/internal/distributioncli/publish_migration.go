package distributioncli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	putserverclient "go.putnami.dev/cloud/clients/put-server/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

const configAuthoredMemberMediaType = "application/vnd.putnami.config.authored-member+json"

var immutablePutPart = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,254}$`)

// checkConfigMemberPublication is the immutable Put identity rule a Config
// member must meet before it is published or packed into the publication
// outbox.
func checkConfigMemberPublication(publication ConfigMemberPublication) error {
	if !validImmutablePutMember(publication.Namespace, publication.Package, publication.Version, publication.Payload) {
		return clicore.NewError("Config member publication has an invalid immutable Put identity", clicore.ExitUsage)
	}
	return nil
}

// validImmutablePutMember reports whether a closed member publication names one
// canonical native Put package, a bounded version that cannot escape its URL
// path segment, and a non-empty payload within the 4 MiB manifest bound.
func validImmutablePutMember(namespace, pkg, version string, payload []byte) bool {
	return immutablePutPart.MatchString(namespace) && immutablePutPart.MatchString(pkg) &&
		version != "" && len(version) <= 256 && version == strings.TrimSpace(version) &&
		!strings.ContainsAny(version, "/\x00\r\n") && len(payload) != 0 && len(payload) <= 4<<20
}

// ConfigMemberPublication is the closed input Distribution accepts from the
// native Config CLI producer. Distribution selects the Put origin and bearer;
// callers cannot use this contract as an arbitrary HTTP or credential broker.
type ConfigMemberPublication struct {
	Namespace string
	Package   string
	Version   string
	Payload   []byte
}

// ConfigMemberPublicationResult is the exact immutable Put identity confirmed
// by the atomic publish response or by authenticated collision readback.
type ConfigMemberPublicationResult struct {
	Coordinate     string
	Version        string
	ArtifactDigest string
}

// PublishConfigMember publishes one canonical Config-authored member to the
// existing selected Put endpoint. It never advances a channel.
func PublishConfigMember(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO, publication ConfigMemberPublication) (*ConfigMemberPublicationResult, error) {
	if err := checkConfigMemberPublication(publication); err != nil {
		return nil, err
	}
	credential, err := resolveReleaseSetProviderCredential(params, workspaceRoot, env, ioctx)
	if err != nil {
		return nil, err
	}
	ctx := ioctx.Context
	if ctx == nil {
		ctx = context.Background()
	}
	publisher, err := newPutPublisher(ioctx.Client, credential.Endpoint.URL, credential.Token)
	if err != nil {
		return nil, err
	}
	digest, err := publishImmutablePutManifest(
		ctx, publisher, publication.Namespace, publication.Package, publication.Version, configAuthoredMemberMediaType,
		publication.Payload, "Config member publication",
	)
	if err != nil {
		return nil, err
	}
	return &ConfigMemberPublicationResult{
		Coordinate: publication.Namespace + "/" + publication.Package,
		Version:    publication.Version, ArtifactDigest: digest,
	}, nil
}

// publishImmutablePutManifest publishes without a channel and returns the
// digest of the exact stored manifest bytes. Fresh publishes validate the
// manifest in the atomic response; conflicts require an authenticated readback
// before they can be treated as idempotent success.
func publishImmutablePutManifest(ctx context.Context, publisher *putPublisher, ns, pkg, version, mediaType string, payload []byte, subject string) (string, error) {
	noChannel := ""
	callCtx, refusal := publisher.call(ctx)
	stored, err := publisher.put.CreatePutPublish(callCtx, putserverclient.CreatePutPublishInput{
		Path: putserverclient.CreatePutPublishPath{Namespace: ns, Package: pkg},
		Body: putserverclient.AtomicPublishRequest{
			Version: &version, MediaType: &mediaType, Payload: json.RawMessage(payload), Channel: &noChannel,
		},
	})
	if clicore.ServiceStatus(err) == http.StatusConflict {
		return readBackImmutablePutManifest(ctx, publisher, ns, pkg, version, mediaType, payload, subject)
	}
	if invalidPutAnswer(err) {
		return "", clicore.NewError(subject+" returned an invalid atomic publish response", clicore.ExitAPI)
	}
	if err != nil {
		return "", putLegError(subject, refusal, err)
	}
	return verifyAtomicPutPublishResponse(stored, ns, pkg, version, mediaType, payload, subject)
}

func verifyAtomicPutPublishResponse(stored *putserverclient.AtomicPublishResponse, ns, pkg, version, mediaType string, payload []byte, subject string) (string, error) {
	if stored == nil || stored.Version == nil || stored.Manifest == nil {
		return "", clicore.NewError(subject+" returned an invalid atomic publish response", clicore.ExitAPI)
	}
	expectedPackage := ns + "/" + pkg
	storedVersion, storedManifest := stored.Version, stored.Manifest
	if stringValue(stored.Package) != expectedPackage || stringValue(storedVersion.Id) == "" || stringValue(storedVersion.PackageId) == "" ||
		stringValue(storedVersion.Version) != version || stringValue(storedVersion.ManifestId) == "" ||
		stringValue(storedVersion.State) != "published" || stringValue(storedVersion.Visibility) != "private" ||
		stringValue(storedManifest.Id) != stringValue(storedVersion.ManifestId) ||
		stringValue(storedManifest.PackageId) != stringValue(storedVersion.PackageId) ||
		!stored.Channel.IsNull() {
		return "", clicore.NewError(subject+" returned a different immutable package or version identity", clicore.ExitAPI)
	}
	return verifyImmutablePutManifest(storedManifest, mediaType, payload, subject)
}

func readBackImmutablePutManifest(ctx context.Context, publisher *putPublisher, ns, pkg, version, mediaType string, payload []byte, subject string) (string, error) {
	callCtx, refusal := publisher.call(ctx)
	stored, err := publisher.put.GetPutVersionsManifest(callCtx, putserverclient.GetPutVersionsManifestInput{
		Path: putserverclient.GetPutVersionsManifestPath{Namespace: ns, Package: pkg, Version: version},
	})
	if invalidPutAnswer(err) {
		return "", clicore.NewError(subject+" collision readback returned an invalid manifest", clicore.ExitAPI)
	}
	if err != nil {
		return "", putLegError(subject+" collision readback", refusal, err)
	}
	return verifyImmutablePutManifest(stored, mediaType, payload, subject+" collision")
}

func verifyImmutablePutManifest(stored *putserverclient.Manifest, mediaType string, payload []byte, subject string) (string, error) {
	if stored == nil || stringValue(stored.Id) == "" || stringValue(stored.PackageId) == "" ||
		stringValue(stored.MediaType) != mediaType || !bytes.Equal(stored.Payload, payload) {
		return "", clicore.NewError(subject+" does not match the immutable version already stored at that coordinate", clicore.ExitAPI)
	}
	return digestOfBytes(stored.Payload), nil
}

func digestOfBytes(payload []byte) string {
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// resolvePutCredentials returns the Put registry (put-server) base URL and bearer token.
// URL: --registry-put-url / PUTNAMI_REGISTRY_PUT_URL override, else the default
// production host. Token: the put auth recorded by `putnami cloud registries`
// for that host, exchanged for a short-lived registry JWT.
//
// put-server validates RS256 JWTs (aud=distribution), not the raw pkt_* api key,
// so the stored key must be exchanged the same way `cloud registry-token` does
// before it is presented as the bearer — sending the opaque key straight through
// makes the auth middleware fail closed with 401 "invalid credentials".
func resolvePutCredentials(params map[string]any, env map[string]string, ioctx clicore.IO) (baseURL, token string, err error) {
	baseURL = clicore.FirstString(
		clicore.StringParam(params, "registry-put-url", "registryPutUrl"),
		clicore.EnvGet(env, "PUTNAMI_REGISTRY_PUT_URL"),
		defaultRegistryURL[RegistryPut],
	)
	host := hostFromURL(baseURL)
	state, err := readRegistriesState(env)
	if err != nil {
		return "", "", err
	}
	var apiKey string
	if state != nil {
		apiKey = state.PutAuth[host]
	}
	if apiKey == "" {
		return "", "", clicore.NewError(
			"no Put registry credential for "+host+" — run `putnami cloud login` (or `putnami cloud registries setup`) first",
			clicore.ExitUsage)
	}
	if isDistributionLeaseBearer(apiKey) {
		return baseURL, apiKey, nil
	}
	endpoint, ok := registryEndpointForHost(params, env, state, host)
	if !ok {
		endpoint = RegistryEndpoint{Registry: RegistryPut, URL: baseURL, Host: host}
	}
	token, err = exchangeRegistryTokenJWT(params, env, ioctx, endpoint, apiKey)
	if err != nil {
		return "", "", err
	}
	return baseURL, token, nil
}

func setBearer(req *http.Request, token string) {
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func doJSON(client *http.Client, req *http.Request) (*http.Response, []byte, error) {
	clicore.SetUserAgent(req)
	resp, err := client.Do(req) //nolint:gosec // G704: CLI intentionally requests the user-configured Putnami Cloud endpoint
	if err != nil {
		return nil, nil, clicore.NewError(fmt.Sprintf("request failed for %s: %s", req.URL, err.Error()), clicore.ExitAPI)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, clicore.NewError("read response: "+err.Error(), clicore.ExitAPI)
	}
	return resp, body, nil
}
