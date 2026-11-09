package ruby

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/protocol/job"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// defaultHost is what the stub ruby answers the host probe with: Ruby 3.3.6,
// shared, on glibc Linux x86_64, with the preprocessor flags -DSTUB.
const defaultHost = `3.3.6\nx86_64\nlinux\n\n3.3.0\n-DSTUB`

// rubyTools puts first on PATH a ruby 3.3.6 and a bundle 2.5.22. The ruby
// answers the host probe with STUB_RUBY_HOST, else defaultHost. The bundle
// logs every call but --version and check to the returned log, and the
// BUNDLE_, PORT and CONFIGURE_ARGS variables of each to the log's .env file.
// Its install writes a bundle under BUNDLE_PATH, with the extension directory
// STUB_EXTENSION_API names and what puma's native build leaves, and its cache
// a .gem file in BUNDLE_CACHE_PATH. It exits with STUB_EXIT.
func rubyTools(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "bundle.log")
	writeFile(t, bin, "ruby", `#!/bin/sh
if [ "$1" = "-e" ]; then printf "${STUB_RUBY_HOST:-`+defaultHost+`}"; exit 0; fi
echo 'ruby 3.3.6 (2024-11-05 revision 75015d4c1f) [x86_64-linux]'
`)
	writeFile(t, bin, "bundle", `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'Bundler version 2.5.22'; exit 0; fi
if [ "$1" = "check" ]; then exit "${STUB_BUNDLE_CHECK:-0}"; fi
echo "$PWD: bundle $*" >> "`+log+`"
echo "$(env | grep -E '^(BUNDLE_[A-Z_]*|PORT|CONFIGURE_ARGS)=' | sort | tr '\n' ' ')" >> "`+log+`.env"
if [ "$1" = "install" ] && [ -n "$BUNDLE_PATH" ]; then
  gems="$BUNDLE_PATH/ruby/3.3.0"
  mkdir -p "$gems/gems/rack-3.1.0/lib" "$gems/cache" "$gems/extensions/x86_64-linux/${STUB_EXTENSION_API:-3.3.0}/puma-6.4.0" "$gems/bundler/gems/tool-abc123/.git" "$gems/bin" "$gems/gems/puma-6.4.0/ext/puma_http11"
  for f in gem_make.out mkmf.log; do echo "$BUNDLE_PATH" > "$gems/extensions/x86_64-linux/${STUB_EXTENSION_API:-3.3.0}/puma-6.4.0/$f"; done
  for f in Makefile puma_http11.o; do echo "$BUNDLE_PATH" > "$gems/gems/puma-6.4.0/ext/puma_http11/$f"; done
  echo 'ELF' > "$gems/extensions/x86_64-linux/${STUB_EXTENSION_API:-3.3.0}/puma-6.4.0/puma_http11.so"
  echo 'int x;' > "$gems/gems/puma-6.4.0/ext/puma_http11/puma_http11.c"
  echo 'module Rack; end' > "$gems/gems/rack-3.1.0/lib/rack.rb"
  echo 'gem' > "$gems/cache/rack-3.1.0.gem"
  echo '' > "$gems/extensions/x86_64-linux/${STUB_EXTENSION_API:-3.3.0}/puma-6.4.0/gem.build_complete"
  echo 'ref' > "$gems/bundler/gems/tool-abc123/.git/HEAD"
  echo 'module Tool; end' > "$gems/bundler/gems/tool-abc123/tool.rb"
  printf '#!/usr/bin/env ruby\n' > "$gems/bin/rackup"
  chmod 755 "$gems/bin/rackup"
fi
if [ "$1" = "cache" ] && [ -n "$BUNDLE_CACHE_PATH" ]; then
  mkdir -p "$BUNDLE_CACHE_PATH"
  echo 'gem' > "$BUNDLE_CACHE_PATH/rack-3.1.0.gem"
fi
exit "${STUB_EXIT:-0}"
`)
	for _, f := range []string{"ruby", "bundle"} {
		if err := os.Chmod(filepath.Join(bin, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("STUB_EXIT", "")
	t.Setenv("STUB_RUBY_HOST", "")
	t.Setenv("STUB_EXTENSION_API", "")
	return log
}

// rackApp writes a Rack application with its own Gemfile at rel under root.
func rackApp(t *testing.T, root, rel string) {
	t.Helper()
	writeFile(t, root, rel+"/Gemfile", "source \"https://rubygems.org\"\n\ngem \"rack\"\ngem \"puma\"\n")
	writeFile(t, root, rel+"/Gemfile.lock", "GEM\n  remote: https://rubygems.org/\n  specs:\n    rack (3.1.0)\n\nPLATFORMS\n  ruby\n\nBUNDLED WITH\n   2.5.22\n")
	writeFile(t, root, rel+"/.ruby-version", "3.3.6\n")
	writeFile(t, root, rel+"/config.ru", "run ->(env) { [200, {}, [\"ok\"]] }\n")
}

// workspaceRoot answers a new workspace directory by its real path: on macOS
// the temporary directory is a symbolic link, and the steps' $PWD is the real
// path.
func workspaceRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// runProjectJob runs a job for the component at rel, with the type and the
// metadata the probe answers for it.
func runProjectJob(t *testing.T, root, rel string, jobFunc cli.JobFunc, params job.Params, args ...string) (string, map[string]any, error) {
	t.Helper()
	s := stack(t)
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{rel}})
	if err != nil || len(result.Projects) != 1 {
		t.Fatalf("probe: %v %+v", err, result)
	}
	ctx := &pctx.Context{WorkspaceRoot: root, Params: params, Project: pctx.Project{
		Name: rel, Path: rel, FullPath: filepath.Join(root, rel), Type: result.Projects[0].Type,
		Metadata: map[string]json.RawMessage{ExtensionName: result.Projects[0].Metadata},
	}}
	return quietly(t, func() (string, map[string]any, error) { return jobFunc(ctx, jsonl.New(), args) })
}

// quietly runs f with the job protocol on stdout sent nowhere.
func quietly(t *testing.T, f func() (string, map[string]any, error)) (string, map[string]any, error) {
	t.Helper()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	stdout := os.Stdout
	os.Stdout = devnull
	defer func() { os.Stdout = stdout }()
	return f()
}

func readLines(t *testing.T, file string) []string {
	t.Helper()
	content, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(content)), "\n")
}

func TestServeRunsRailsServerOrRackupOnAnApplication(t *testing.T) {
	spectest.Proves(t, feature, "serves-an-application", "serve-runs-rails-server-or-rackup-on-an-application")
	log := rubyTools(t)
	root := workspaceRoot(t)
	rackApp(t, root, "shop")

	status, _, err := runProjectJob(t, root, "shop", ServeJob(stack(t)), nil)
	if status != "OK" || err != nil {
		t.Fatalf("serve: %s %v", status, err)
	}
	writeFile(t, root, "shop/bin/rails", "#!/usr/bin/env ruby\n")
	status, _, err = runProjectJob(t, root, "shop", ServeJob(stack(t)), job.Params{"port": json.RawMessage(`8081`)})
	if status != "OK" || err != nil {
		t.Fatalf("serve: %s %v", status, err)
	}
	shop := filepath.Join(root, "shop")
	lines := readLines(t, log)
	if len(lines) != 2 || lines[0] != shop+": bundle exec rackup -o 0.0.0.0 -p 3000" || lines[1] != shop+": bundle exec rails server -b 0.0.0.0 -p 8081" {
		t.Errorf("bundle ran with %q", lines)
	}
	if env := readLines(t, log+".env"); len(env) != 2 || !strings.Contains(env[0], "PORT=3000") || !strings.Contains(env[1], "PORT=8081") {
		t.Errorf("env = %q", env)
	}
}

func TestServeOnPortZeroPicksAFreePort(t *testing.T) {
	log := rubyTools(t)
	root := workspaceRoot(t)
	rackApp(t, root, "shop")
	if status, _, err := runProjectJob(t, root, "shop", ServeJob(stack(t)), nil, "--port", "0"); status != "OK" || err != nil {
		t.Fatalf("serve: %s %v", status, err)
	}
	lines := readLines(t, log)
	if len(lines) != 1 {
		t.Fatalf("bundle ran with %q", lines)
	}
	fields := strings.Fields(lines[0])
	if port, err := strconv.Atoi(fields[len(fields)-1]); err != nil || port == 0 {
		t.Errorf("port = %q", fields[len(fields)-1])
	}
}

func TestServeNeedsARackConfig(t *testing.T) {
	rubyTools(t)
	root := workspaceRoot(t)
	rackApp(t, root, "shop")
	s := stack(t)
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{"shop"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "shop", rackConfig)); err != nil {
		t.Fatal(err)
	}
	ctx := &pctx.Context{WorkspaceRoot: root, Project: pctx.Project{
		Name: "shop", Path: "shop", FullPath: filepath.Join(root, "shop"), Type: TypeApplication,
		Metadata: map[string]json.RawMessage{ExtensionName: result.Projects[0].Metadata},
	}}
	status, _, err := quietly(t, func() (string, map[string]any, error) { return ServeJob(s)(ctx, jsonl.New(), nil) })
	if status != "FAILED" || err == nil || err.Error() != CodeNoRackConfig {
		t.Errorf("serve: %s %v", status, err)
	}
}

func TestServeFailsWhenTheServerExitsOnItsOwn(t *testing.T) {
	rubyTools(t)
	root := workspaceRoot(t)
	rackApp(t, root, "shop")
	t.Setenv("STUB_EXIT", "5")
	status, data, err := runProjectJob(t, root, "shop", ServeJob(stack(t)), nil)
	if status != "FAILED" || err != nil || data[exitCodeKey] != 5 {
		t.Errorf("serve: %s %v %v", status, data, err)
	}
}

func TestRunForwardsTheExitCodeOfTheEntrypoint(t *testing.T) {
	spectest.Proves(t, feature, "runs-an-application-once", "run-forwards-the-exit-code-of-the-entrypoint")
	log := rubyTools(t)
	root := workspaceRoot(t)
	rackApp(t, root, "shop")
	t.Setenv("STUB_EXIT", "7")

	status, data, err := runProjectJob(t, root, "shop", RunJob(stack(t)), job.Params{"args": json.RawMessage(`"db:migrate RAILS_ENV=test"`)})
	if status != "FAILED" || err != nil || data[exitCodeKey] != 7 {
		t.Fatalf("run: %s %v %v", status, data, err)
	}
	t.Setenv("STUB_EXIT", "")
	status, data, err = runProjectJob(t, root, "shop", RunJob(stack(t)), job.Params{"entrypoint": json.RawMessage(`"bin/job.rb"`)})
	if status != "OK" || err != nil || data[exitCodeKey] != 0 {
		t.Fatalf("run: %s %v %v", status, data, err)
	}
	shop := filepath.Join(root, "shop")
	if lines := readLines(t, log); len(lines) != 2 || lines[0] != shop+": bundle exec ruby bin/rails db:migrate RAILS_ENV=test" || lines[1] != shop+": bundle exec ruby bin/job.rb" {
		t.Errorf("bundle ran with %q", lines)
	}
}

func TestServeRunAndPackageSkipALibrary(t *testing.T) {
	log := rubyTools(t)
	root := fixture(t)
	for name, jobFunc := range map[string]cli.JobFunc{"serve": ServeJob(stack(t)), "run": RunJob(stack(t)), "package": PackageDockerJob(stack(t))} {
		if status, _, err := runProjectJob(t, root, "packs/pricing", jobFunc, nil); status != "SKIP" || err != nil {
			t.Errorf("%s: %s %v", name, status, err)
		}
	}
	if lines := readLines(t, log); lines != nil {
		t.Errorf("bundle ran: %q", lines)
	}
}

func TestADirectoryWithConfigRuIsAnApplication(t *testing.T) {
	root := fixture(t)
	rackApp(t, root, "apps/admin")
	s := stack(t)
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{"apps/admin", "packs/pricing", "gems/audit_log"}})
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]string{}
	for _, p := range result.Projects {
		types[p.Path] = p.Type
	}
	if types["apps/admin"] != TypeApplication || types["packs/pricing"] != TypeLibrary || types["gems/audit_log"] != TypeLibrary {
		t.Errorf("types = %v", types)
	}
}
