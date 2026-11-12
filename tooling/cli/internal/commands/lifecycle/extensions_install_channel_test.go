package lifecycle

import (
	"errors"
	"slices"
	"testing"

	"go.putnami.dev/tooling/cli/internal/cmderr"
)

// TestExtensionsInstallChannelPinsTheNamedExtension checks that `extensions
// install --channel <channel> <name>` installs <name>@<channel>, as init
// installs its extensions, and that a channel with nothing to apply it to is a
// usage error rather than ignored (#1087).
func TestExtensionsInstallChannelPinsTheNamedExtension(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    []string
		wantErr bool
	}{
		{name: "a separate value", args: []string{"--channel", "canary", "@putnami/observe"},
			want: []string{"@putnami/observe@canary"}},
		{name: "an attached value after the name", args: []string{"@putnami/observe", "--channel=canary", "--latest"},
			want: []string{"@putnami/observe@canary", "--latest"}},
		{name: "beside a materialization", args: []string{"--platform", "linux/amd64", "--dest", "out", "@putnami/observe", "--channel", "next"},
			want: []string{"--platform", "linux/amd64", "--dest", "out", "@putnami/observe@next"}},
		{name: "stable reads latest", args: []string{"--channel", "stable", "@putnami/observe"},
			want: []string{"@putnami/observe"}},
		{name: "no channel", args: []string{"@putnami/observe"}, want: []string{"@putnami/observe"}},
		{name: "no name", args: []string{"--channel", "canary"}, wantErr: true},
		{name: "a local extension", args: []string{"--channel", "canary", "./tools/ext"}, wantErr: true},
		{name: "a name with its version", args: []string{"--channel", "canary", "@putnami/observe@1.2.3"}, wantErr: true},
		{name: "an exact version", args: []string{"--channel", "1.2.3", "@putnami/observe"}, wantErr: true},
		{name: "no value", args: []string{"@putnami/observe", "--channel"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExtensionsInstallChannel(tt.args)
			if tt.wantErr {
				if !errors.Is(err, cmderr.ErrUsage) {
					t.Fatalf("ExtensionsInstallChannel(%v) = %v, %v, want a usage error", tt.args, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExtensionsInstallChannel(%v): %v", tt.args, err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("ExtensionsInstallChannel(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}
