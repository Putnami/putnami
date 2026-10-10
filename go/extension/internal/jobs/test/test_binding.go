package test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.putnami.dev/go/extension/internal/jobs/configmerge"
	"go.putnami.dev/go/extension/internal/toolchain"
	pdb "go.putnami.dev/protocol/database"
	diag "go.putnami.dev/protocol/diagnostic"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/dbtestenv"
	"go.putnami.dev/sdk/extension/hostenv"
	"gopkg.in/yaml.v3"
)

// buildTestEnv assembles the `go test` subprocess environment, resolving the
// database test binding with the precedence the contract states:
//
//  1. An EXTERNALLY set DATABASE_TEST_BINDINGS always wins and is forwarded
//     untouched. It is also the value this task declares as a `{"from": "env"}`
//     cache input, so it folds into the test job's cache key by itself.
//  2. The invocation-scoped artifact `test-env-up` provisioned for this run.
//
// It is a `sensitive` runtime-file living at mode
//
//	   0600 in the invocation's private tree, delivered through the job
//	   context's non-secret `invocation` locator — never through an inherited
//	   environment variable, which every descendant process and `ps` can see.
//	   The consuming job's cache key folds the PRODUCING ACTION's digest, so the
//	   credential's bytes are never a cache input.
//	3. The project's committed conf datasources, synthesized here as before.
func buildTestEnv(ctx *pctx.Context, projectPath string, race bool, goBinary string) ([]string, error) {
	// Base the test env on WorkspaceBuildEnv, not a raw os.Environ(): an
	// inherited GOWORK=off is stripped and GOWORK points at the governing go.work.
	//
	// Then drop the HOST's platform identity block. The scrub applies to the
	// INHERITED environment only: every setEnv below runs after it and still wins.
	//
	// Then drop the variables that describe this job to the extension on a
	// hosted run (hostenv.JobVars). WorkspaceBuildEnv already read the offline
	// signal and turned module downloads off, and those go settings stay: the
	// repository's tests run offline, but they are not jobs of the run.
	env := hostenv.ScrubJobVars(hostenv.ScrubPlatformIdentity(toolchain.WorkspaceBuildEnv(os.Environ(), projectPath, goBinary)))
	envName := testEnvironment()
	env = setEnv(env, envAppEnv, envName)
	env = setEnv(env, "FORCE_COLOR", "1")
	if race {
		// The Go race detector requires cgo. CI and reproducible build
		// environments commonly pin CGO_ENABLED=0, so a race-enabled project
		// must override that ambient build setting for its host test process.
		env = setEnv(env, "CGO_ENABLED", "1")
	}

	if strings.TrimSpace(os.Getenv(envDatabaseTestBindings)) != "" {
		return env, nil
	}

	if provisioned, ok := dbtestenv.BindingFrom(ctx); ok {
		return setEnv(env, envDatabaseTestBindings, provisioned), nil
	}

	binding, ok, err := loadDatabaseTestBinding(projectPath, envName)
	if err != nil {
		return nil, err
	}
	if ok {
		env = setEnv(env, envDatabaseTestBindings, binding)
	}
	return env, nil
}

func testEnvironment() string {
	if env := strings.TrimSpace(os.Getenv(envAppEnv)); env != "" {
		return env
	}
	return defaultTestEnvironment
}

func loadDatabaseTestBinding(projectPath, envName string) (string, bool, error) {
	config := make(map[string]any)
	for _, rel := range []string{
		filepath.Join("conf", ".env.yaml"),
		filepath.Join("conf", ".env."+envName+".yaml"),
		filepath.Join(".gen", "conf", ".env."+envName+".yaml"),
		filepath.Join("conf", ".secrets."+envName+".yaml"),
	} {
		data, ok, err := loadYAMLConfig(filepath.Join(projectPath, rel))
		if err != nil {
			return "", false, err
		}
		if ok {
			configmerge.DeepMerge(config, data)
		}
	}
	if len(config) == 0 {
		return "", false, nil
	}

	tb, ok, err := databaseTestBindingFromConfig(config)
	if err != nil || !ok {
		return "", ok, err
	}
	raw, err := json.Marshal(tb)
	if err != nil {
		return "", false, fmt.Errorf("marshal %s: %w", envDatabaseTestBindings, err)
	}
	if _, diags := pdb.ParseAndValidateTestBinding(raw); diag.HasErrors(diags) {
		return "", false, fmt.Errorf("invalid %s from test config:\n%s", envDatabaseTestBindings, diag.ErrorText(diags))
	}
	return string(raw), true, nil
}

func loadYAMLConfig(path string) (map[string]any, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var out map[string]any
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", path, err)
	}
	return out, true, nil
}

func databaseTestBindingFromConfig(config map[string]any) (*pdb.TestBinding, bool, error) {
	testSection, _ := asMap(config["databaseTest"])
	if rawBinding, ok := canonicalTestBinding(testSection); ok {
		return rawBinding, true, nil
	}

	databaseSection, ok := asMap(config["database"])
	if !ok || len(databaseSection) == 0 {
		return nil, false, nil
	}

	tb := &pdb.TestBinding{
		ProtocolVersion: pdb.ProtocolVersion,
		Databases:       make(map[string]pdb.Database),
	}
	applyTestPolicy(tb, testSection)

	for name, rawEntry := range normalizeDatabaseEntries(databaseSection) {
		entry, ok := asMap(rawEntry)
		if !ok {
			continue
		}
		db, ok, err := databaseEntryFromConfig(entry)
		if err != nil {
			return nil, false, err
		}
		if ok {
			tb.Databases[name] = db
		}
	}
	if len(tb.Databases) == 0 {
		return nil, false, nil
	}
	return tb, true, nil
}

func canonicalTestBinding(section map[string]any) (*pdb.TestBinding, bool) {
	if len(section) == 0 {
		return nil, false
	}
	if _, ok := section["databases"]; !ok {
		return nil, false
	}
	raw, err := json.Marshal(section)
	if err != nil {
		return nil, false
	}
	var tb pdb.TestBinding
	if err := json.Unmarshal(raw, &tb); err != nil {
		return nil, false
	}
	if tb.ProtocolVersion == 0 {
		tb.ProtocolVersion = pdb.ProtocolVersion
	}
	return &tb, true
}

func normalizeDatabaseEntries(databaseSection map[string]any) map[string]any {
	if looksLikeConnection(databaseSection) {
		return map[string]any{"default": databaseSection}
	}
	return databaseSection
}

func databaseEntryFromConfig(entry map[string]any) (pdb.Database, bool, error) {
	connectionSource := entry
	if nested, ok := asMap(entry["connection"]); ok {
		connectionSource = nested
	}
	if !looksLikeConnection(connectionSource) {
		return pdb.Database{}, false, nil
	}

	conn, err := connectionFromConfig(connectionSource)
	if err != nil {
		return pdb.Database{}, false, err
	}
	return pdb.Database{
		Engine:     pdb.Engine(firstString(entry, "engine", string(pdb.EnginePostgres))),
		Schema:     firstString(entry, "schema", "public"),
		Connection: conn,
	}, true, nil
}

func connectionFromConfig(entry map[string]any) (*pdb.Connection, error) {
	conn := &pdb.Connection{
		DSN:      stringValue(entry["dsn"]),
		Host:     stringValue(entry["host"]),
		Database: stringValue(entry["database"]),
		User:     stringValue(entry["user"]),
		Password: stringValue(entry["password"]),
		Instance: stringValue(entry["instance"]),
	}
	if port, ok := intValue(entry["port"]); ok {
		conn.Port = port
	}
	if ssl, ok := boolValue(entry["ssl"]); ok {
		conn.SSL = &ssl
	}
	if params, ok, err := stringMap(entry["params"]); err != nil {
		return nil, err
	} else if ok {
		conn.Params = params
	}
	return conn, nil
}

func applyTestPolicy(tb *pdb.TestBinding, section map[string]any) {
	if len(section) == 0 {
		return
	}
	tb.Mode = pdb.TestMode(stringValue(section["mode"]))
	tb.Isolation = pdb.Isolation(stringValue(section["isolation"]))
	tb.Reuse = pdb.Reuse(stringValue(section["reuse"]))
	if apply, ok := boolValue(section["applyMigrations"]); ok {
		tb.ApplyMigrations = apply
	}
	if keep, ok := boolValue(section["keepDatabases"]); ok {
		tb.KeepDatabases = keep
	}
}

func looksLikeConnection(m map[string]any) bool {
	for _, key := range []string{"dsn", "host", "instance", "connection"} {
		if _, ok := m[key]; ok {
			return true
		}
	}
	return false
}

func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func firstString(m map[string]any, key, fallback string) string {
	if value := strings.TrimSpace(stringValue(m[key])); value != "" {
		return value
	}
	return fallback
}

func stringValue(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case fmt.Stringer:
		return strings.TrimSpace(x.String())
	default:
		return ""
	}
}

func intValue(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	default:
		return 0, false
	}
}

func boolValue(v any) (bool, bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	default:
		return false, false
	}
}

func stringMap(v any) (map[string]string, bool, error) {
	m, ok := asMap(v)
	if !ok {
		return nil, false, nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		value := stringValue(v)
		if value == "" {
			return nil, false, fmt.Errorf("database params.%s must be a string", k)
		}
		out[k] = value
	}
	return out, true, nil
}

// withGoTempDir routes the go command's work directories (go-build*) into a
// "gotmp" directory under parent, a scratch-owned directory. The go command
// removes its work directory on exit, but not when it is killed; inside owned
// scratch, what a killed go command leaves is reclaimed with its parent. A
// non-empty GOTMPDIR in env is kept; one set only with `go env -w` is not
// consulted. On error env is returned unchanged.
func withGoTempDir(env []string, parent string) ([]string, error) {
	last := -1
	for i, entry := range env {
		if strings.HasPrefix(entry, "GOTMPDIR=") {
			last = i
		}
	}
	if last >= 0 && env[last] != "GOTMPDIR=" {
		return env, nil
	}
	dir := filepath.Join(parent, "gotmp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return env, fmt.Errorf("create Go work directory scratch: %w", err)
	}
	if last >= 0 {
		env[last] = "GOTMPDIR=" + dir
		return env, nil
	}
	return append(env, "GOTMPDIR="+dir), nil
}

func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}
