package memory

import (
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestARemoteURLWithAQueryOrFragmentIsRefused(t *testing.T) {
	spectest.Proves(t, feature, "explicit-backend", "a-remote-url-with-a-query-or-fragment-is-refused")
	for _, remote := range []string{
		"https://host.example/acme/memory.git?token=s3cret",
		"https://host.example/acme/memory.git?",
		"https://host.example/acme/memory.git#s3cret",
		"ssh://git@host.example/acme/memory.git?key=s3cret",
	} {
		failure := checkRemote(remote)
		if failure == nil || failure.Error.Reason != "settings.invalid" {
			t.Errorf("remote %s: %+v", remote, failure)
		}
	}
	for _, remote := range []string{"origin", "https://host.example/acme/memory.git", "ssh://git@host.example/acme/memory.git",
		"git@host.example:acme/memory.git", "../memory.git"} {
		if failure := checkRemote(remote); failure != nil {
			t.Errorf("remote %s refused: %+v", remote, failure)
		}
	}
}
