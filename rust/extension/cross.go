package rust

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/pinnedarchive"
	"go.putnami.dev/tooling/stacks"
)

const (
	zigVersion      = "0.17.1"
	zigbuildVersion = "0.24.1"
)

// wheel is a pinned PyPI wheel, which is a zip file.
type wheel struct {
	url    string
	sha256 string
}

// crossTool is a tool the package compile runs. subdir is the directory of the
// wheel that holds its binary; zig finds its library files beside the binary.
type crossTool struct {
	name    string
	binary  string
	version string
	subdir  string
	wheels  map[string]wheel
}

const pypiFiles = "https://files.pythonhosted.org/packages/"

// zigbuildDarwin is one universal wheel for both macOS architectures.
var zigbuildDarwin = wheel{pypiFiles + "49/2e/a29ea1d3da84e9a41fc29cc23d4b66bb88968853255cfe2ba218c86186b4/cargo_zigbuild-0.24.1-py3-none-macosx_10_12_universal2.macosx_10_12_x86_64.macosx_11_0_arm64.whl", "7b832d824bbf852b9a99b02a0dfa2166a00462fc237c2387d15a67c385755795"}

// crossTools are the tools of the package compile. Tests point them at a local
// server.
var crossTools = []crossTool{
	{
		name: "zig", binary: "zig", version: zigVersion, subdir: "ziglang",
		wheels: map[string]wheel{
			"darwin/arm64":  {pypiFiles + "ea/e1/4a9a97324fcf2d292c14a2e0121ea0df86c01d0c307ea24fa35aebaccd15/ziglang-0.17.1-py3-none-macosx_13_0_arm64.whl", "09953b6af7f64168b915180a42774f520f92d86ef6331b28e33915c7e1f0bb23"},
			"darwin/amd64":  {pypiFiles + "17/f6/f4dfc8cf0e510980fda251a8edf99823b9446f5d7a2a6a24801e55e09c28/ziglang-0.17.1-py3-none-macosx_13_0_x86_64.whl", "4e0b4b9c02b8d0d220e9948010f860b62ff11d96abb39762eca5fa2ef3f7bc97"},
			"linux/amd64":   {pypiFiles + "59/e3/2a34676370a11c36b7cd04ffb2439c1089c0530cf4b01408c77cf7f8d9c3/ziglang-0.17.1-py3-none-manylinux_2_12_x86_64.manylinux2010_x86_64.musllinux_1_1_x86_64.whl", "ea6cc5d87b033cf0aedfbc8f9549a908cae6c50ac31f1698188fa48a74524b54"},
			"linux/arm64":   {pypiFiles + "91/72/bb7c4f098ba3e535fa2ebefd9f26327ba63fb0672562959255cb5dd37aff/ziglang-0.17.1-py3-none-manylinux_2_17_aarch64.manylinux2014_aarch64.musllinux_1_1_aarch64.whl", "b7c5f02fccd40f89b999ee16a1f5ddced56e6182370f8affe494bcf8a28cf07e"},
			"windows/amd64": {pypiFiles + "c8/6c/2013114fa780f0cf490cdc98bdd5bf7dce706a5dbd86e50ec4132cf7d4b1/ziglang-0.17.1-py3-none-win_amd64.whl", "69c3dfc3aff6a297ec4017aa904782c0e450eb988982d7f675d83edce27526fb"},
			"windows/arm64": {pypiFiles + "70/34/a1d775ceaf325b08c3aca909865e057919d0fe1355e2b59081500ef230a1/ziglang-0.17.1-py3-none-win_arm64.whl", "27a8da5f8eaa0e76ebbc7c2594130ed4646c1101ada2ce0aa86fe580c74154b9"},
		},
	},
	{
		name: "cargo-zigbuild", binary: "cargo-zigbuild", version: zigbuildVersion, subdir: "cargo_zigbuild-0.24.1.data/scripts",
		wheels: map[string]wheel{
			"darwin/arm64":  zigbuildDarwin,
			"darwin/amd64":  zigbuildDarwin,
			"linux/amd64":   {pypiFiles + "19/bd/5476661915fd4fb8bc6eb439ac2c3682a6454cfd3efb0365832e02897556/cargo_zigbuild-0.24.1-py3-none-manylinux1_x86_64.manylinux_2_5_x86_64.whl", "238a1ab54a95053a4eadc34775c81f795a8ad75f80bfed63f3f445f518102375"},
			"linux/arm64":   {pypiFiles + "98/74/69f6f26cb6ba003c3946940afe69828354a8c78781b74651b550851f58e3/cargo_zigbuild-0.24.1-py3-none-manylinux2014_aarch64.manylinux_2_17_aarch64.whl", "07c5e3dcddf208500462ec927c556bb306cca2b533b0abe29446bffb63767b85"},
			"windows/amd64": {pypiFiles + "41/54/4f6cc6ed8b11cfb0a9330cfe6ff35832252e0df827c71a65c6bef28acfea/cargo_zigbuild-0.24.1-py3-none-win_amd64.whl", "46e0ce74fb1bd36486d9fa28e1df33530d390ce2d1cdbab0019d844bdfdfebdf"},
			"windows/arm64": {pypiFiles + "06/98/1b6f718c639d906dbd718ca50aa4e4afacc46891ff35d22ff0b05489f421/cargo_zigbuild-0.24.1-py3-none-win_arm64.whl", "e1bb2f0d888395161c7e3957d771d5806432728cf6303fdb536d9103b375812c"},
		},
	},
}

// compiler is what the package compile runs: the cargo of the installed Rust,
// cargo-zigbuild, and the environment that makes them find each other.
type compiler struct {
	cargo    string
	zigbuild string
	env      []string
}

// provideCompiler installs the Rust the project expects, zig, cargo-zigbuild
// and the standard library of each target, always in the Putnami home: adding
// a target needs rustup, which a Rust on PATH may lack. With no network it
// fails naming what is missing. Tests replace it.
var provideCompiler = func(ctx context.Context, expected stacks.Expected, targets, env []string, info func(string)) (compiler, error) {
	channel, triple, err := resolveChannel(expected)
	if err != nil {
		return compiler{}, err
	}
	offline := os.Getenv(extensionproto.OfflineDependenciesEnv) == "1"
	if !offline {
		if _, err := Provide(ctx, expected, info); err != nil {
			return compiler{}, err
		}
	}
	base, release, err := stacks.LockedToolsHome("rust")
	if err != nil {
		return compiler{}, err
	}
	defer release()
	rustupHome, toolchain, provided := toolchainPaths(base, channel, triple)
	var missing []string
	if _, err := os.Stat(filepath.Join(toolchain, installedMarker)); err != nil {
		missing = append(missing, "Rust "+channel)
	}
	dirs := map[string]string{}
	for _, t := range crossTools {
		dir, err := provideCrossTool(ctx, base, t, offline, info)
		if err != nil {
			return compiler{}, err
		}
		if dir == "" {
			missing = append(missing, t.name+" "+t.version)
		}
		dirs[t.name] = dir
	}
	absent := missingTargets(toolchain, targets)
	if offline {
		for _, target := range absent {
			missing = append(missing, "the Rust target "+target)
		}
		if len(missing) > 0 {
			return compiler{}, fmt.Errorf("package needs a network to install %s", strings.Join(missing, ", "))
		}
	} else if len(absent) > 0 {
		rustup, err := provideRustup(ctx, base, triple)
		if err != nil {
			return compiler{}, err
		}
		info("installing the Rust targets " + strings.Join(absent, ", "))
		name := channel + "-" + triple
		cmd := exec.CommandContext(ctx, rustup, append([]string{"target", "add", "--toolchain", name}, absent...)...) //nolint:gosec // the pinned rustup, with checked targets
		cmd.Dir = base
		cmd.Env = append(withoutRustupEnv(os.Environ()), "RUSTUP_HOME="+rustupHome)
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return compiler{}, fmt.Errorf("rustup target add: %w", err)
		}
	}
	bin := filepath.Join(toolchain, "bin")
	// The first directory wins: this cargo and rustc, whatever the machine has.
	return compiler{
		cargo:    filepath.Join(bin, "cargo"+stacks.ExecutableSuffix(hostPlatform)),
		zigbuild: filepath.Join(dirs["cargo-zigbuild"], "cargo-zigbuild"+stacks.ExecutableSuffix(hostPlatform)),
		env:      prependPath(append(withoutRustupEnv(env), provided.Env...), bin, dirs["cargo-zigbuild"], dirs["zig"]),
	}, nil
}

// provideCrossTool returns the directory of a tool's binary, installing the
// tool when it is missing. With no network, a missing tool answers "".
func provideCrossTool(ctx context.Context, base string, t crossTool, offline bool, info func(string)) (string, error) {
	dest := filepath.Join(base, t.name+"-"+t.version)
	subdir := filepath.FromSlash(t.subdir)
	binary := filepath.Join(subdir, t.binary+stacks.ExecutableSuffix(hostPlatform))
	if _, err := os.Stat(filepath.Join(dest, binary)); err == nil {
		return filepath.Join(dest, subdir), nil
	}
	if offline {
		return "", nil
	}
	pin, ok := t.wheels[hostPlatform]
	if !ok {
		return "", fmt.Errorf("no %s is pinned for %s", t.name, hostPlatform)
	}
	installed, err := pinnedarchive.Install(ctx, pinnedarchive.Pin{URL: pin.url, SHA256: pin.sha256, Format: pinnedarchive.Zip}, dest, pinnedarchive.Options{
		Limits: pinnedarchive.DefaultLimits(),
		Complete: func(candidate string) bool {
			_, statErr := os.Stat(filepath.Join(candidate, binary))
			return statErr == nil
		},
		// A wheel need not record that its binary is executable.
		Prepare: func(stage string) error { return os.Chmod(filepath.Join(stage, binary), 0o755) }, //nolint:gosec // an executable
	})
	if err != nil {
		return "", fmt.Errorf("install %s %s: %w", t.name, t.version, err)
	}
	if installed {
		info("installed " + t.name + " " + t.version + " under " + dest)
	}
	return filepath.Join(dest, subdir), nil
}

// missingTargets are the targets whose standard library the toolchain lacks.
func missingTargets(toolchain string, targets []string) []string {
	var missing []string
	for _, target := range targets {
		if _, err := os.Stat(filepath.Join(toolchain, "lib", "rustlib", target)); err != nil {
			missing = append(missing, target)
		}
	}
	return missing
}

// prependPath puts dirs first on the PATH of env.
func prependPath(env []string, dirs ...string) []string {
	joined := strings.Join(dirs, string(os.PathListSeparator))
	out := make([]string, 0, len(env)+1)
	found := false
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(key, "PATH") {
			found = true
			entry = key + "=" + joined + string(os.PathListSeparator) + value
		}
		out = append(out, entry)
	}
	if !found {
		out = append(out, "PATH="+joined)
	}
	return out
}
