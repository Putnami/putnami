package stacks

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/putnamihome"
)

func TestLockedToolsHomeLocksTheStackFolderAndRemovesStaleStaging(t *testing.T) {
	home := t.TempDir()
	t.Setenv(putnamihome.Env, home)
	stale := filepath.Join(home, "tools", "demo", "download-1234")
	kept := filepath.Join(home, "tools", "demo", "1.2.3")
	for _, dir := range []string{stale, kept} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	base, release, err := LockedToolsHome("demo")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if base != filepath.Join(home, "tools", "demo") {
		t.Errorf("base = %s", base)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a staging folder an install left survives: %v", err)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("an installed release is removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, ".lock")); err != nil {
		t.Errorf("no lock file: %v", err)
	}
}

func TestLockedToolsHomeNeedsAPutnamiHome(t *testing.T) {
	t.Setenv(putnamihome.Env, "")
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	if home := putnamihome.Resolve(os.Getenv); home != "" {
		t.Skipf("the host resolves a Putnami home without the variables: %s", home)
	}
	if _, _, err := LockedToolsHome("demo"); err == nil {
		t.Error("no Putnami home is an error")
	}
}

func TestExecutableSuffixIsExeOnWindowsOnly(t *testing.T) {
	for platform, want := range map[string]string{"windows/amd64": ".exe", "linux/amd64": "", "darwin/arm64": ""} {
		if got := ExecutableSuffix(platform); got != want {
			t.Errorf("%s: %q, want %q", platform, got, want)
		}
	}
}

func TestCompareVersionsOrdersDottedNumbers(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"3.10.0", "3.9.2", 1},
		{"6.1", "6.1.0", 0},
		{"1.2.3", "1.2.10", -1},
		{"2", "10", -1},
	} {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

type pinned struct{ version string }

func (p pinned) release() string { return p.version }

func TestAcceptedKeepsTheOrderAndChooseFollowsThePreferences(t *testing.T) {
	all := []pinned{{"17"}, {"21"}, {"25"}}
	accepted := Accepted(all, pinned.release, func(v string) bool { return v != "25" })
	if !reflect.DeepEqual(accepted, []pinned{{"17"}, {"21"}}) {
		t.Fatalf("accepted = %v", accepted)
	}
	if got, ok := Choose(accepted, pinned.release, "17", "21"); !ok || got.version != "17" {
		t.Errorf("the first preference wins: %v %v", got, ok)
	}
	if got, ok := Choose(accepted, pinned.release, "", "21"); !ok || got.version != "21" {
		t.Errorf("an empty preference is skipped: %v %v", got, ok)
	}
	if got, ok := Choose(accepted, pinned.release, "11"); !ok || got.version != "21" {
		t.Errorf("with no accepted preference, the newest: %v %v", got, ok)
	}
	if _, ok := Choose([]pinned{}, pinned.release, "21"); ok {
		t.Error("nothing accepted chooses nothing")
	}
}

func TestAnswerToolchainResolvePinsItsIdentityOnly(t *testing.T) {
	pin := wsproto.LockToolchainEntry{Version: "3.4.1"}
	calls := 0
	resolve := func(root string, _ wsproto.ToolchainResolveRequest) (wsproto.LockToolchainEntry, bool) {
		calls++
		return pin, root == "/repo"
	}
	request := wsproto.ToolchainResolveRequest{Identities: []string{"ruby", "go"}}
	result := AnswerToolchainResolve("/repo", request, "@putnami/ruby", "ruby", resolve)
	if result.Extension != "@putnami/ruby" || result.Version != wsproto.ToolchainResolveProtocolVersion {
		t.Errorf("result header: %+v", result)
	}
	if got := result.Toolchains["ruby"].Pin; got == nil || got.Version != "3.4.1" {
		t.Errorf("ruby pin: %+v", result.Toolchains["ruby"])
	}
	if problem := result.Toolchains["go"].Problem; problem == nil || problem.Message == "" {
		t.Errorf("another identity is a problem: %+v", result.Toolchains["go"])
	}
	if calls != 1 {
		t.Errorf("resolve ran %d times", calls)
	}
	if empty := AnswerToolchainResolve("/elsewhere", wsproto.ToolchainResolveRequest{Identities: []string{"ruby"}}, "@putnami/ruby", "ruby", resolve); len(empty.Toolchains) != 0 {
		t.Errorf("an identity it pins nothing for is omitted: %+v", empty.Toolchains)
	}
}

func TestServeToolchainResolveAnswersOnlyItsControlCall(t *testing.T) {
	var out strings.Builder
	handled, err := ServeToolchainResolve([]string{"lint"}, strings.NewReader(""), &out, "@putnami/ruby", "ruby", nil)
	if handled || err != nil || out.Len() != 0 {
		t.Errorf("another command is not handled: %v %v %q", handled, err, out.String())
	}
}
