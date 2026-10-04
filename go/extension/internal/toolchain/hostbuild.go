package toolchain

import (
	pctx "go.putnami.dev/sdk/extension/context"
)

// HostBuild is the ONE configuration of a project's host `go build`, shared by
// every step that compiles the project's own entrypoint for the machine the
// job runs on: the compile phase of build~compile, and the describe binary of
// build~describe (which the `test`, `serve`, `run` and `package` pipelines
// schedule too).
//
// Why one configuration: build~describe is a full `go build` of the workload's
// entrypoint — the Describer plugins run inside the binary — and build~compile
// builds the same entrypoint right after it, on the same host, through the
// same GOCACHE. Go serves the second build from that cache only for the
// packages whose ACTION ID matches, and every package's action ID folds the
// build's CGO_ENABLED, -tags, -gcflags, -asmflags, -race and -trimpath, plus
// the content ID of each dependency. Describe used to force CGO_ENABLED=0 and
// pass no flag at all, so on a host with a C compiler — CGO_ENABLED=1 by
// default since Go 1.20, which is every hosted Linux runner — `net` and
// `runtime/cgo` were built differently and every package above them
// (`net/http`, the framework's `app`, `http` and `api`, the workload itself)
// compiled a second time. Compile then cost as much as describe.
// Resolved once, the two builds are one program and compile is a link.
//
// -ldflags is deliberately not part of the shared set: linker flags enter the
// LINK action only, never a compile action, so leaving the `-X` version stamp
// out of describe still shares every compile action with the stamped build,
// and keeps a version bump from re-linking the describe binary.
type HostBuild struct {
	// Mod is the module download mode (`-mod`); Readonly selects `readonly`
	// when Mod says nothing.
	Mod      string
	Readonly bool
	Gcflags  string
	Asmflags string
	Tags     string
	Race     bool
	Trimpath bool
	// Installsuffix isolates the build cache (`-installsuffix`).
	Installsuffix string
	// Parallelism is `-p`, kept as the string the parameter arrived as.
	Parallelism string
	Buildmode   string
	// CGO is the tri-state `cgo` parameter: "" keeps the host default, "true"
	// sets CGO_ENABLED=1, "false" sets CGO_ENABLED=0.
	CGO string
	// Buildvcs is the tri-state `buildvcs` parameter: "" keeps Go's default
	// (`auto`, which stamps the checkout's revision, time and modified state
	// into the binary), "true" and "false" pass `-buildvcs=true` and
	// `-buildvcs=false`. A binary built with false is the same bytes at every
	// commit whose sources it compiles are unchanged.
	Buildvcs string
}

// HostBuildParamNames is the closed set of parameters ResolveHostBuild reads,
// and the contract a task owes its cache: a task whose job builds with a
// HostBuild must, for every name here, either PIN the value in its manifest
// argv (argv wins below, so a pinned value cannot be moved by a parameter) or
// declare it as a `from: "params"` input. Anything else lets two runs that
// build different programs share one cache entry.
//
// TestHostBuildParamNamesCoversEveryReadParameter proves this list is what
// ResolveHostBuild actually reads, and the Go extension's manifest contract
// test walks it against build-compile, build-cross-compile and build-describe,
// so a flag added here cannot silently escape either check.
//
// `ldflags` is deliberately absent: it is not part of the shared configuration
// (see HostBuild), and the one phase that links with it passes it to BuildArgs
// itself.
var HostBuildParamNames = []string{
	"mod",
	"readonly",
	"gcflags",
	"asmflags",
	"tags",
	"race",
	"trimpath",
	"installsuffix",
	"p",
	"buildmode",
	"cgo",
	"buildvcs",
}

// ResolveHostBuild reads the host build configuration the way every job of
// this extension receives its parameters.
//
// flags are the job's own argv flags (cli.ParseFlags) and win when present.
// ctx.Params is the bag the orchestrator actually delivers: a task's argv is
// fixed by its manifest entry (`build --phase compile`, `build-describe`), so
// a project option or a command flag reaches the job through the resolved
// parameters and nowhere else. Reading both, in this order, is what lets the
// describe step and the compile step agree — one resolver, one bag.
func ResolveHostBuild(ctx *pctx.Context, flags map[string]string) HostBuild {
	var params pctx.Params
	if ctx != nil {
		params = ctx.Params
	}
	str := func(key string) string {
		if v, ok := flags[key]; ok {
			return v
		}
		return params.String(key)
	}
	boolean := func(key string) bool {
		if v, ok := flags[key]; ok {
			return flagBool(v)
		}
		return params.Bool(key, false)
	}
	host := HostBuild{
		Mod:           str("mod"),
		Readonly:      boolean("readonly"),
		Gcflags:       str("gcflags"),
		Asmflags:      str("asmflags"),
		Tags:          str("tags"),
		Race:          boolean("race"),
		Trimpath:      boolean("trimpath"),
		Installsuffix: str("installsuffix"),
		Parallelism:   str("p"),
		Buildmode:     str("buildmode"),
	}
	// `cgo` is presence-sensitive: false forces CGO_ENABLED=0, which is not
	// what saying nothing means.
	if v, ok := flags["cgo"]; ok {
		host.CGO = cgoValue(flagBool(v))
	} else if _, ok := params["cgo"]; ok {
		host.CGO = cgoValue(params.Bool("cgo", false))
	}
	// `buildvcs` is presence-sensitive for the same reason: false strips the
	// VCS stamp, saying nothing keeps it.
	if v, ok := flags["buildvcs"]; ok {
		host.Buildvcs = cgoValue(flagBool(v))
	} else if _, ok := params["buildvcs"]; ok {
		host.Buildvcs = cgoValue(params.Bool("buildvcs", false))
	}
	return host
}

// Args returns the `go build` flags of this configuration, without -ldflags.
// This is what describe builds with; see HostBuild for why the linker flags
// are not shared.
func (h HostBuild) Args() []string {
	return h.BuildArgs("")
}

// BuildArgs returns Args plus the caller's -ldflags, in the position the
// compile and cross-compile phases have always emitted them.
func (h HostBuild) BuildArgs(ldflags string) []string {
	var args []string
	mod := h.Mod
	if mod == "" && h.Readonly {
		mod = "readonly"
	}
	if mod != "" {
		args = append(args, "-mod", mod)
	}
	if h.Gcflags != "" {
		args = append(args, "-gcflags", h.Gcflags)
	}
	if h.Asmflags != "" {
		args = append(args, "-asmflags", h.Asmflags)
	}
	if ldflags != "" {
		args = append(args, "-ldflags", ldflags)
	}
	if h.Tags != "" {
		args = append(args, "-tags", h.Tags)
	}
	if h.Race {
		args = append(args, "-race")
	}
	if h.Trimpath {
		args = append(args, "-trimpath")
	}
	if h.Installsuffix != "" {
		args = append(args, "-installsuffix", h.Installsuffix)
	}
	if h.Parallelism != "" {
		args = append(args, "-p", h.Parallelism)
	}
	if h.Buildmode != "" {
		args = append(args, "-buildmode", h.Buildmode)
	}
	if h.Buildvcs != "" {
		args = append(args, "-buildvcs="+h.Buildvcs)
	}
	return args
}

// Env returns the environment of a host build of the module at projectDir:
// WorkspaceBuildEnv, then CGO_ENABLED as the `cgo` parameter forces it — left
// at the host default when the parameter is absent.
//
// -race forces CGO_ENABLED=1 last, over an explicit `cgo: false`: the rule
// `go test -race` applies (internal/jobs/test/test_binding.go).
func (h HostBuild) Env(env []string, projectDir, goBinary string) []string {
	out := WorkspaceBuildEnv(env, projectDir, goBinary)
	switch h.CGO {
	case "true":
		out = setEnvValue(out, "CGO_ENABLED", "1")
	case "false":
		out = setEnvValue(out, "CGO_ENABLED", "0")
	}
	if h.Race {
		out = setEnvValue(out, "CGO_ENABLED", "1")
	}
	return out
}

// HostBuildInvocation is the single call the compile phase and the describe
// binary both make: the `go build` flags and the environment of this
// project's host build, from one resolution of one parameter bag. The caller
// adds `-o`, the package, and — the compile phase only — its -ldflags.
func HostBuildInvocation(ctx *pctx.Context, flags map[string]string, env []string, projectDir, goBinary string) (args, hostEnv []string) {
	host := ResolveHostBuild(ctx, flags)
	return host.Args(), host.Env(env, projectDir, goBinary)
}

func cgoValue(enabled bool) string {
	if enabled {
		return "true"
	}
	return "false"
}

// flagBool reads an argv boolean exactly as the SDK's cli.FlagBool does, so a
// value spelled on the command line and the same value resolved as a
// parameter cannot disagree.
func flagBool(v string) bool {
	switch v {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}
