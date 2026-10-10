package clicore

import "testing"

func TestRepositoryKeyFromRemote(t *testing.T) {
	for _, tc := range []struct {
		remote string
		want   string
	}{
		{"https://github.com/acme/app.git", "acme/app"},
		{"ssh://git@github.com/acme/app.git", "acme/app"},
		{"git@github.com:acme/app.git", "acme/app"},
		{"git://github.com/acme/app", "acme/app"},
	} {
		provider, got, err := RepositoryKeyFromRemote(tc.remote)
		if err != nil || provider != "github" || got != tc.want {
			t.Errorf("RepositoryKeyFromRemote(%q) = %q, %q, %v; want github, %q", tc.remote, provider, got, err, tc.want)
		}
	}
	if _, _, err := RepositoryKeyFromRemote("acme/app"); err == nil {
		t.Fatal("expected a hostless remote to fail")
	}
}
