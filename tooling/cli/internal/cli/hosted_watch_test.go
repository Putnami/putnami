package cli

import (
	"errors"
	"testing"
)

// A hosted run that watches is refused as a usage error: --watch, and serve,
// which always watches. Every watch iteration starts after repository code
// ran, when no process may receive the run credential. A finite hosted run,
// and any run without the credential, is not refused.
func TestAHostedRunRefusesToWatch(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		hosted   bool
		watch    bool
		commands []string
		refused  bool
	}{
		"hosted --watch":       {hosted: true, watch: true, commands: []string{"build"}, refused: true},
		"hosted serve":         {hosted: true, commands: []string{"serve"}, refused: true},
		"hosted build,serve":   {hosted: true, commands: []string{"build", "serve"}, refused: true},
		"hosted build":         {hosted: true, commands: []string{"build"}},
		"hosted run":           {hosted: true, commands: []string{"run"}},
		"local --watch":        {watch: true, commands: []string{"build"}},
		"local serve":          {commands: []string{"serve"}},
		"local build --watch":  {watch: true, commands: []string{"build", "test"}},
		"hosted lint,test,run": {hosted: true, commands: []string{"lint", "test", "run"}},
	} {
		err := refuseHostedWatch(tc.hosted, &GlobalFlags{Watch: tc.watch}, tc.commands)
		if !tc.refused {
			if err != nil {
				t.Errorf("%s: refused: %v", name, err)
			}
			continue
		}
		if !errors.Is(err, ErrHostedWatch) {
			t.Errorf("%s: want ErrHostedWatch, got %v", name, err)
		}
		if code := exitCodeForError(err); code != ExitUsage {
			t.Errorf("%s: exit code %d, want %d", name, code, ExitUsage)
		}
	}
}
