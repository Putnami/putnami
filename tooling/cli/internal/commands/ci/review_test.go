package ci

import (
	"errors"
	"reflect"
	"testing"

	ciproto "go.putnami.dev/protocol/ci"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestCIReviewProfileValidatesWithoutProvider(t *testing.T) {
	spectest.Proves(t, "cli/ci-document", "review-guidance-is-bounded-intent", "review-profile-validates-without-provider")
	root := newCICommandWorkspace(t)
	cfg := wsproto.Load(root)
	workspace.InvalidateLoadCache(root)
	writeCIDocument(t, root, `{"version":3,"commands":["test"],"review":{"enabled":true,"fallbackEngine":"codex","focus":["security"],"instructions":["Check workspace isolation."]}}`)
	before, err := ciproto.Parse(readCIDocument(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := captureCICommand(func() error { return CIValidate(root, cfg, "jsonl") }); err != nil {
		t.Fatal(err)
	}
	if _, err := captureCICommand(func() error { return CIFormat(root, cfg, false, "jsonl") }); err != nil {
		t.Fatal(err)
	}
	after, err := ciproto.Parse(readCIDocument(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("format changed review guidance or CI commands")
	}
}

func TestCIReviewAuthorityFieldsAreRefused(t *testing.T) {
	spectest.Proves(t, "cli/ci-document", "review-guidance-is-bounded-intent", "review-authority-fields-are-refused")
	root := newCICommandWorkspace(t)
	cfg := wsproto.Load(root)
	workspace.InvalidateLoadCache(root)
	writeCIDocument(t, root, `{"version":3,"commands":["test"],"review":{"enabled":true,"fallbackEngine":"codex","focus":["security"],"instructions":[],"command":"run untrusted code"}}`)
	_, err := captureCICommand(func() error { return CIValidate(root, cfg, "jsonl") })
	if !errors.Is(err, cmderr.ErrInvalidConfig) {
		t.Fatalf("unsafe review accepted: %v", err)
	}
}
