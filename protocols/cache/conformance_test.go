package cache

import (
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// runConformance loads every fixture under fixtures/<kind>/{valid,invalid} and
// asserts the parse+validate outcome matches the directory.
func runConformance(t *testing.T, kind string, parse func([]byte) (any, []diag.Diagnostic)) {
	t.Helper()

	check := func(dir string, wantErrors bool) {
		glob := filepath.Join("fixtures", kind, dir, "*.json")
		files, err := filepath.Glob(glob)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) == 0 {
			t.Fatalf("no fixtures found for %s", glob)
		}
		for _, path := range files {
			t.Run(filepath.Join(kind, dir, filepath.Base(path)), func(t *testing.T) {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				v, diags := parse(data)
				if wantErrors && !diag.HasErrors(diags) {
					t.Errorf("invalid fixture %s should produce errors but none found", path)
				}
				if !wantErrors {
					if diag.HasErrors(diags) {
						t.Errorf("valid fixture %s produced errors: %v", path, diags)
					}
					if v == nil {
						t.Errorf("valid fixture %s returned nil", path)
					}
				}
			})
		}
	}

	check("valid", false)
	check("invalid", true)
}

func TestConformance_Request(t *testing.T) {
	runConformance(t, "request", func(b []byte) (any, []diag.Diagnostic) {
		r, d := ParseAndValidateRequest(b)
		if r == nil {
			return nil, d
		}
		return r, d
	})
}

func TestConformance_Response(t *testing.T) {
	runConformance(t, "response", func(b []byte) (any, []diag.Diagnostic) {
		r, d := ParseAndValidateResponse(b)
		if r == nil {
			return nil, d
		}
		return r, d
	})
}

func TestConformance_StoreRequest(t *testing.T) {
	runConformance(t, "store-request", func(b []byte) (any, []diag.Diagnostic) {
		r, d := ParseAndValidateStoreRequest(b)
		if r == nil {
			return nil, d
		}
		return r, d
	})
}

func TestConformance_StoreResponse(t *testing.T) {
	runConformance(t, "store-response", func(b []byte) (any, []diag.Diagnostic) {
		r, d := ParseAndValidateStoreResponse(b)
		if r == nil {
			return nil, d
		}
		return r, d
	})
}

func TestConformance_CommitRequest(t *testing.T) {
	runConformance(t, "commit-request", func(b []byte) (any, []diag.Diagnostic) {
		r, d := ParseAndValidateCommitRequest(b)
		if r == nil {
			return nil, d
		}
		return r, d
	})
}

func TestConformance_CommitResponse(t *testing.T) {
	runConformance(t, "commit-response", func(b []byte) (any, []diag.Diagnostic) {
		r, d := ParseAndValidateCommitResponse(b)
		if r == nil {
			return nil, d
		}
		return r, d
	})
}

func TestConformance_CapabilitiesResponse(t *testing.T) {
	runConformance(t, "capabilities-response", func(b []byte) (any, []diag.Diagnostic) {
		r, d := ParseAndValidateCapabilitiesResponse(b)
		if r == nil {
			return nil, d
		}
		return r, d
	})
}

func TestConformance_FindMissingRequest(t *testing.T) {
	runConformance(t, "find-missing-request", func(b []byte) (any, []diag.Diagnostic) {
		r, d := ParseAndValidateFindMissingRequest(b)
		if r == nil {
			return nil, d
		}
		return r, d
	})
}

func TestConformance_FindMissingResponse(t *testing.T) {
	runConformance(t, "find-missing-response", func(b []byte) (any, []diag.Diagnostic) {
		r, d := ParseAndValidateFindMissingResponse(b)
		if r == nil {
			return nil, d
		}
		return r, d
	})
}

func TestConformance_CommitBatchRequest(t *testing.T) {
	runConformance(t, "commit-batch-request", func(b []byte) (any, []diag.Diagnostic) {
		r, d := ParseAndValidateCommitBatchRequest(b)
		if r == nil {
			return nil, d
		}
		return r, d
	})
}

func TestConformance_CommitBatchResponse(t *testing.T) {
	runConformance(t, "commit-batch-response", func(b []byte) (any, []diag.Diagnostic) {
		r, d := ParseAndValidateCommitBatchResponse(b)
		if r == nil {
			return nil, d
		}
		return r, d
	})
}

func TestConformance_UploadGrant(t *testing.T) {
	runConformance(t, "upload-grant", func(b []byte) (any, []diag.Diagnostic) {
		r, d := ParseAndValidateUploadGrant(b)
		if r == nil {
			return nil, d
		}
		return r, d
	})
}

func TestConformance_UploadBatchRequest(t *testing.T) {
	runConformance(t, "upload-batch-request", func(b []byte) (any, []diag.Diagnostic) {
		r, d := ParseAndValidateUploadBatchRequest(b)
		if r == nil {
			return nil, d
		}
		return r, d
	})
}

func TestConformance_UploadBatchResponse(t *testing.T) {
	runConformance(t, "upload-batch-response", func(b []byte) (any, []diag.Diagnostic) {
		r, d := ParseAndValidateUploadBatchResponse(b)
		if r == nil {
			return nil, d
		}
		return r, d
	})
}

func TestConformance_DownloadBatchRequest(t *testing.T) {
	runConformance(t, "download-batch-request", func(b []byte) (any, []diag.Diagnostic) {
		r, d := ParseAndValidateDownloadBatchRequest(b)
		if r == nil {
			return nil, d
		}
		return r, d
	})
}

func TestConformance_DownloadBatchResponse(t *testing.T) {
	runConformance(t, "download-batch-response", func(b []byte) (any, []diag.Diagnostic) {
		r, d := ParseAndValidateDownloadBatchResponse(b)
		if r == nil {
			return nil, d
		}
		return r, d
	})
}

// The HTTP run-marker request/response family (cache.go:241, strict_runmarker.go)
// shipped strict parsers and only response-side unit tests — the request shapes
// and the whole valid/invalid corpus had no fixtures, so a relaxation of a
// run-marker validation rule (version, required key, marker sha) passed the
// conformance suite. These four tests wire an HTTP-shape corpus in. They are
// distinct from the provider RPC marker shapes (fixtures/marker-*), which carry
// found/published instead of protocolVersion and are covered by
// provider_conformance_test.go.

func TestConformance_RunMarkerRequest(t *testing.T) {
	runConformance(t, "run-marker-request", func(b []byte) (any, []diag.Diagnostic) {
		r, d := ParseAndValidateRunMarkerRequest(b)
		if r == nil {
			return nil, d
		}
		return r, d
	})
}

func TestConformance_RunMarkerResponse(t *testing.T) {
	runConformance(t, "run-marker-response", func(b []byte) (any, []diag.Diagnostic) {
		r, d := ParseAndValidateRunMarkerResponse(b)
		if r == nil {
			return nil, d
		}
		return r, d
	})
}

func TestConformance_PublishRunMarkerRequest(t *testing.T) {
	runConformance(t, "publish-run-marker-request", func(b []byte) (any, []diag.Diagnostic) {
		r, d := ParseAndValidatePublishRunMarkerRequest(b)
		if r == nil {
			return nil, d
		}
		return r, d
	})
}

func TestConformance_PublishRunMarkerResponse(t *testing.T) {
	runConformance(t, "publish-run-marker-response", func(b []byte) (any, []diag.Diagnostic) {
		r, d := ParseAndValidatePublishRunMarkerResponse(b)
		if r == nil {
			return nil, d
		}
		return r, d
	})
}
