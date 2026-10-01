package launch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

const (
	// probeTimeout bounds one run of the pinned CLI's help.
	probeTimeout = 30 * time.Second
	// probeWaitDelay bounds how long the probe waits for the help output once
	// the pinned CLI has exited or been killed.
	probeWaitDelay = 2 * time.Second
	// probeOutputLimit bounds how much help output the probe keeps. The top
	// level help is about 10 KiB.
	probeOutputLimit = 1 << 20
	// noAutoInstallEnv turns off the first-use install of the probed CLI.
	noAutoInstallEnv = "PUTNAMI_NO_AUTO_INSTALL"
)

// probeInheritedEnv names the only variables the probed CLI receives from
// this process. They let it start and find nothing else.
var probeInheritedEnv = []string{"PATH", "HOME"}

// ErrPinBelowCustodyLevel names the refusal of a hosted relaunch: the pinned
// CLI advertises a custody level below this CLI's runcredential.CustodyLevel,
// or none.
var ErrPinBelowCustodyLevel = errors.New("the pinned CLI is below this CLI's custody level")

// probeResult is what the probe reads from the pinned CLI's help.
type probeResult struct {
	// listed reports that a line of the help starts with runcredential.Flag.
	listed bool
	// level is the custody level that line advertises, or 0.
	level int
}

// credentialProbe returns the probe a hosted relaunch runs on the pinned CLI
// before it hands that CLI the run credential. The probe runs `<path> --help`
// on the digest-verified store binary and reads, from the line that starts
// with runcredential.Flag, the custody level the CLI advertises
// (runcredential.AdvertisedCustodyLevel). A help that exits non-zero is an
// error.
//
// The probe gives the pinned CLI nothing to act on. It runs from an empty
// temporary directory, so it finds no workspace and no workspace code runs.
// Its environment holds only probeInheritedEnv, NoRelaunchEnv and
// noAutoInstallEnv, so it neither relaunches nor installs, and it sees no
// capability, provider choice or bound request of this run. It receives only
// the three standard streams: stdin is empty, stderr is discarded, and no
// descriptor carries the run credential, which stays in memory until the
// exec. getenv reads the inherited variables.
func credentialProbe(getenv func(string) string) func(ctx context.Context, path string) (probeResult, error) {
	return func(ctx context.Context, path string) (probeResult, error) {
		dir, err := os.MkdirTemp("", "putnami-credential-probe-")
		if err != nil {
			return probeResult{}, err
		}
		defer func() { _ = os.RemoveAll(dir) }()
		ctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		out := &cappedBuffer{limit: probeOutputLimit}
		cmd := exec.CommandContext(ctx, path, "--help")
		cmd.Dir = dir
		cmd.Env = probeEnv(getenv)
		cmd.Stdout = out
		cmd.WaitDelay = probeWaitDelay
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return probeResult{}, fmt.Errorf("its help did not finish within %s: %w", probeTimeout, err)
			}
			return probeResult{}, err
		}
		level, listed := runcredential.AdvertisedCustodyLevel(out.Bytes())
		return probeResult{listed: listed, level: level}, nil
	}
}

// probeEnv is the probed CLI's whole environment.
func probeEnv(getenv func(string) string) []string {
	env := make([]string, 0, len(probeInheritedEnv)+2)
	for _, name := range probeInheritedEnv {
		if value := getenv(name); value != "" {
			env = append(env, name+"="+value)
		}
	}
	return append(env, NoRelaunchEnv+"=1", noAutoInstallEnv+"=1")
}

// cappedBuffer keeps the first limit bytes written to it and discards the
// rest, so a runaway child cannot grow it without bound.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// Bytes returns the kept bytes.
func (b *cappedBuffer) Bytes() []byte {
	return b.buf.Bytes()
}

// olderPinError refuses a pinned CLI below this CLI's custody level: one that
// does not accept runcredential.Flag, or advertises a lower level, or none.
// It wraps ErrPinBelowCustodyLevel and is InvalidConfig because the pin is
// what has to change. `putnami pin` is exempt from the relaunch, so the
// running CLI can move the pin.
func olderPinError(version string, probed probeResult) error {
	pinned := "does not accept " + runcredential.Flag
	switch {
	case probed.listed && probed.level == 0:
		pinned = "advertises no custody level"
	case probed.listed:
		pinned = fmt.Sprintf("advertises custody level %d", probed.level)
	}
	return protocolcli.WithNext(
		protocolcli.Classify(fmt.Errorf(
			"%w: %s pins CLI %q, which %s; a hosted run hands the run credential only to a CLI at "+
				"custody level %d or above, so move the pin to one",
			ErrPinBelowCustodyLevel, lockfile.LockFilename, version, pinned, runcredential.CustodyLevel),
			protocolcli.ErrInvalidConfig),
		"putnami pin <version>")
}

// probeError reports a pinned CLI whose help could not be read, so the launcher
// cannot establish its custody level. Like execError it names the store path,
// because the binary that would not run is that file.
func probeError(c config, version, path string, err error) error {
	return protocolcli.WithNext(
		fmt.Errorf("could not check that pinned CLI %q from %s is at custody level %d, as %s requires: %w",
			version, path, runcredential.CustodyLevel, runcredential.Flag, err),
		optOutCommand(c.args))
}
