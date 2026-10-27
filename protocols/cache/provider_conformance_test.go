package cache

import (
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

// conformanceFn adapts a typed ParseAndValidate function to the untyped shape
// runConformance expects, returning a nil any (not a typed nil pointer) when
// parsing/validation fails so the helper's nil check fires correctly.
func conformanceFn[T any](f func([]byte) (*T, []diag.Diagnostic)) func([]byte) (any, []diag.Diagnostic) {
	return func(b []byte) (any, []diag.Diagnostic) {
		v, d := f(b)
		if v == nil {
			return nil, d
		}
		return v, d
	}
}

func TestConformance_ProviderRequest(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "the-fixture-corpus-is-the-contract", "every-provider-fixture-gets-its-verdict")
	runConformance(t, "provider-request", conformanceFn(ParseAndValidateProviderRequest))
}

func TestConformance_ProviderResponse(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "the-fixture-corpus-is-the-contract", "every-provider-fixture-gets-its-verdict")
	runConformance(t, "provider-response", conformanceFn(ParseAndValidateProviderResponse))
}

func TestConformance_InitializeParams(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "the-fixture-corpus-is-the-contract", "every-provider-fixture-gets-its-verdict")
	runConformance(t, "initialize-params", conformanceFn(ParseAndValidateInitializeParams))
}

func TestConformance_InitializeResult(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "the-fixture-corpus-is-the-contract", "every-provider-fixture-gets-its-verdict")
	runConformance(t, "initialize-result", conformanceFn(ParseAndValidateInitializeResult))
}

func TestConformance_AuthenticateParams(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "the-fixture-corpus-is-the-contract", "every-provider-fixture-gets-its-verdict")
	runConformance(t, "authenticate-params", conformanceFn(ParseAndValidateAuthenticateParams))
}

func TestConformance_AuthenticateResult(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "the-fixture-corpus-is-the-contract", "every-provider-fixture-gets-its-verdict")
	runConformance(t, "authenticate-result", conformanceFn(ParseAndValidateAuthenticateResult))
}

func TestConformance_PrefetchParams(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "the-fixture-corpus-is-the-contract", "every-provider-fixture-gets-its-verdict")
	runConformance(t, "prefetch-params", conformanceFn(ParseAndValidatePrefetchParams))
}

func TestConformance_RestoreParams(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "the-fixture-corpus-is-the-contract", "every-provider-fixture-gets-its-verdict")
	runConformance(t, "restore-params", conformanceFn(ParseAndValidateRestoreParams))
}

func TestConformance_RestoreResult(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "the-fixture-corpus-is-the-contract", "every-provider-fixture-gets-its-verdict")
	runConformance(t, "restore-result", conformanceFn(ParseAndValidateRestoreResult))
}

func TestConformance_PrefetchResult(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "the-fixture-corpus-is-the-contract", "every-provider-fixture-gets-its-verdict")
	runConformance(t, "prefetch-result", conformanceFn(ParseAndValidatePrefetchResult))
}

func TestConformance_UploadResult(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "the-fixture-corpus-is-the-contract", "every-provider-fixture-gets-its-verdict")
	runConformance(t, "upload-result", conformanceFn(ParseAndValidateUploadResult))
}

func TestConformance_MarkerLookupResult(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "the-fixture-corpus-is-the-contract", "every-provider-fixture-gets-its-verdict")
	runConformance(t, "marker-lookup-result", conformanceFn(ParseAndValidateMarkerLookupResult))
}

func TestConformance_MarkerWriteResult(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "the-fixture-corpus-is-the-contract", "every-provider-fixture-gets-its-verdict")
	runConformance(t, "marker-write-result", conformanceFn(ParseAndValidateMarkerWriteResult))
}

func TestConformance_UploadParams(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "the-fixture-corpus-is-the-contract", "every-provider-fixture-gets-its-verdict")
	runConformance(t, "upload-params", conformanceFn(ParseAndValidateUploadParams))
}

func TestConformance_MarkerLookupParams(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "the-fixture-corpus-is-the-contract", "every-provider-fixture-gets-its-verdict")
	runConformance(t, "marker-lookup-params", conformanceFn(ParseAndValidateMarkerLookupParams))
}

func TestConformance_MarkerWriteParams(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "the-fixture-corpus-is-the-contract", "every-provider-fixture-gets-its-verdict")
	runConformance(t, "marker-write-params", conformanceFn(ParseAndValidateMarkerWriteParams))
}

func TestConformance_SummaryResult(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "the-fixture-corpus-is-the-contract", "every-provider-fixture-gets-its-verdict")
	runConformance(t, "summary-result", conformanceFn(ParseAndValidateSummaryResult))
}

// The object-cache corpus is the socket's half of the contract: the two ops a
// job process may issue are the ones a second implementation (the provider
// extension) has to parse identically, so they carry the same valid/invalid
// corpus as every other provider message.

func TestConformance_ObjectGetParams(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "the-fixture-corpus-is-the-contract", "every-provider-fixture-gets-its-verdict")
	runConformance(t, "object-get-params", conformanceFn(ParseAndValidateObjectGetParams))
}

func TestConformance_ObjectGetResult(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "the-fixture-corpus-is-the-contract", "every-provider-fixture-gets-its-verdict")
	runConformance(t, "object-get-result", conformanceFn(ParseAndValidateObjectGetResult))
}

func TestConformance_ObjectPutParams(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "the-fixture-corpus-is-the-contract", "every-provider-fixture-gets-its-verdict")
	runConformance(t, "object-put-params", conformanceFn(ParseAndValidateObjectPutParams))
}

func TestConformance_ObjectPutResult(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "the-fixture-corpus-is-the-contract", "every-provider-fixture-gets-its-verdict")
	runConformance(t, "object-put-result", conformanceFn(ParseAndValidateObjectPutResult))
}
