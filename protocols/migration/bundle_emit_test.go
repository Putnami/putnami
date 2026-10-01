package migration

import (
	"os"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func TestWriteBundle_MaterializesValidBundle(t *testing.T) {
	root := t.TempDir()

	up := []byte("CREATE TABLE iam.users (id UUID PRIMARY KEY);\n")
	down := []byte("DROP TABLE iam.users;\n")
	upPath := "payload/sql/default/iam/0001_init.up.sql"
	downPath := "payload/sql/default/iam/0001_init.down.sql"

	b := Bundle{
		AppName: "wealth",
		Operations: []BundleOperation{{
			Kind:      KindSQL,
			Target:    "default",
			Namespace: "iam",
			Name:      "iam/0001_init",
			Up:        PayloadRef{Path: upPath, Hash: ComputePayloadHash(up)},
			Down:      &PayloadRef{Path: downPath, Hash: ComputePayloadHash(down)},
		}},
	}
	payloads := []BundlePayload{{Path: upPath, Bytes: up}, {Path: downPath, Bytes: down}}

	if err := WriteBundle(root, b, payloads); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}

	// The written manifest parses, validates, carries a digest, and its
	// payloads verify against what landed on disk.
	data, err := os.ReadFile(root + "/" + BundleFileName)
	if err != nil {
		t.Fatal(err)
	}
	parsed, diags := ParseAndValidateBundle(data)
	if diag.HasErrors(diags) {
		t.Fatalf("written manifest is invalid: %v", diags)
	}
	if parsed.Digest == "" {
		t.Fatal("written manifest must carry a digest")
	}
	if vDiags := VerifyBundlePayloads(os.DirFS(root), parsed); diag.HasErrors(vDiags) {
		t.Fatalf("written payloads failed verification: %v", vDiags)
	}
}

func TestWriteBundle_RejectsPayloadMismatch(t *testing.T) {
	root := t.TempDir()
	up := []byte("CREATE TABLE t (id int);\n")
	upPath := "payload/sql/default/app/0001.up.sql"

	b := Bundle{
		AppName: "svc",
		Operations: []BundleOperation{{
			Kind: KindSQL, Target: "default", Name: "app/0001",
			Up:     PayloadRef{Path: upPath, Hash: ComputePayloadHash(up)},
			Safety: SafetySafeOnline,
		}},
	}
	// Supply bytes that do not match the pinned hash.
	payloads := []BundlePayload{{Path: upPath, Bytes: []byte("DROP DATABASE prod;\n")}}

	if err := WriteBundle(root, b, payloads); err == nil {
		t.Fatal("WriteBundle must reject payloads that do not match the manifest")
	}
	if _, err := os.Stat(root + "/" + BundleFileName); !os.IsNotExist(err) {
		t.Fatal("no manifest should be written when payloads do not match")
	}
}

func TestWriteBundle_RejectsMissingPayloadBytes(t *testing.T) {
	root := t.TempDir()
	upPath := "payload/sql/default/app/0001.up.sql"
	b := Bundle{
		AppName: "svc",
		Operations: []BundleOperation{{
			Kind: KindSQL, Target: "default", Name: "app/0001",
			Up: PayloadRef{Path: upPath, Hash: ComputePayloadHash([]byte("x"))},
		}},
	}
	if err := WriteBundle(root, b, nil); err == nil {
		t.Fatal("WriteBundle must reject a manifest whose payloads were not supplied")
	}
}
