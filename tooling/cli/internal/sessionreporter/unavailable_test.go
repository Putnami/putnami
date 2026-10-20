package sessionreporter

import (
	"context"
	"errors"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/sessionstream"
)

// A provider the engine cannot prepare leaves a warning that names an engine
// Cause and echoes no other error, which may carry provider output.
func TestTheUnavailableWarningEchoesOnlyAnEngineCause(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/native-session-reporting", "unavailable-provider", "an-unavailable-provider-echoes-no-other-cause")
	ctx := context.WithValue(context.Background(), credentialKey{}, &credentials{selected: map[string]selection{SessionReporter.Name: {provider: "@test/provider"}}})
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"an engine cause", Cause("the cause"), "session reporter unavailable: the cause; retained session " + holderSessionID + " for replay"},
		{"any other error", errors.New("provider output"), "session reporter unavailable; retained session " + holderSessionID + " for replay"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			log, err := sessionstream.Create(t.TempDir(), holderSessionID)
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			_, err = Start(ctx, SessionReporter, log, holderSessionID, func(context.Context) (LaunchSpec, error) { return LaunchSpec{}, c.err })
			if err == nil || err.Error() != c.want {
				t.Fatalf("Start = %v, want %q", err, c.want)
			}
		})
	}
}
