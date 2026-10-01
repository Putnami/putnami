package skipguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestScanFileRefusedForms(t *testing.T) {
	cases := map[string]struct {
		src  string
		want string
	}{
		"focused test":                                  {"test.only('a', () => {});", "`test.only` focuses the run"},
		"focused describe":                              {"describe.only('a', () => {});", "`describe.only` focuses the run"},
		"focused table":                                 {"it.only.each([1])('a %d', () => {});", "`it.only` focuses the run"},
		"focused table receiver":                        {"describe.each([1]).only('a', () => {});", "`describe.each([1]).only` focuses the run"},
		"focused optional chain":                        {"test?.only('a', () => {});", "`test?.only` focuses"},
		"guarded focus":                                 {"if (process.platform === 'linux') { test.only('a', () => {}); }", "focuses the run"},
		"unguarded skip":                                {"test.skip('a', () => {});", "`test.skip` skips unconditionally"},
		"unguarded describe skip":                       {"describe.skip('a', () => {});", "`describe.skip` skips unconditionally"},
		"unguarded skip table":                          {"test.skip.each([1])('a', () => {});", "`test.skip` skips unconditionally"},
		"skip in a callback":                            {"describe('a', () => {\n  it.skip('b', () => {});\n});", "`it.skip` skips unconditionally"},
		"skip after an if block":                        {"if (x) { y(); }\ntest.skip('a', () => {});", "skips unconditionally"},
		"skipIf reads CI":                               {"describe.skipIf(!!process.env.CI)('a', () => {});", "is guarded by a CI or flakiness condition"},
		"skipIf reads CI by index":                      {"describe.skipIf(process.env['GITHUB_ACTIONS'] === 'true')('a', () => {});", "CI or flakiness condition"},
		"skipIf names flakiness":                        {"it.skipIf(isFlaky)('a', () => {});", "CI or flakiness condition"},
		"if guard names CI":                             {"if (isCI) {\n  test.skip('a', () => {});\n}", "CI or flakiness condition (isCI)"},
		"braceless if names CI":                         {"if (process.env.CI) test.skip('a', () => {});", "CI or flakiness condition"},
		"test.if names flakiness":                       {"test.if(!flakyHost)('a', () => {});", "CI or flakiness condition"},
		"switch on CI":                                  {"switch (process.env.CI) {\n  case 'true':\n    test.skip('a', () => {});\n}", "CI or flakiness condition"},
		"skip after a regex":                            {"const r = /[/]only(/;\ntest.skip('a', () => {});", "skips unconditionally"},
		"skip after a division":                         {"const r = a / b / c;\ntest.skip('a', () => {});", "skips unconditionally"},
		"skip in a template hole":                       {"const s = `${test.skip('a', () => {})}`;", "skips unconditionally"},
		"skip after template brace":                     {"const s = `${ {a: 1}.a }`;\ntest.skip('a', () => {});", "skips unconditionally"},
		"custom test API":                               {"observeMeasurement.skip('a', binding, () => ({}));", "`observeMeasurement.skip` skips unconditionally"},
		"typed arrow body":                              {"observeMeasurement.skip('a', binding, (): Measurement => ({ value: 1 }));", "`observeMeasurement.skip` skips unconditionally"},
		"typed generic arrow body":                      {"observeMeasurement.skip('a', binding, (): Promise<Array<number>> => run());", "`observeMeasurement.skip` skips unconditionally"},
		"test API named *Test":                          {"specTest.skip('a');", "`specTest.skip` skips unconditionally"},
		"direct xit":                                    {"xit('a', () => {});", "`xit` skips unconditionally"},
		"direct xdescribe":                              {"describe('a', () => {\n  xdescribe('b', () => {});\n});", "`xdescribe` skips unconditionally"},
		"direct fit":                                    {"fit('a', () => {});", "`fit` focuses the run, so other tests can stop running; use `it`"},
		"direct fdescribe":                              {"fdescribe(`a`, () => {});", "use `describe`"},
		"skip after JSX in an if":                       {"if (!hasDom) {\n  render(<ul>{items.map((i) => <li key={i}>{i}</li>)}</ul>);\n}\ntest.skip('a', () => {});", "skips unconditionally"},
		"guarded return names CI":                       {"if (process.env.CI) return test.skip('a', () => {});", "CI or flakiness condition"},
		"CI constant":                                   {"if (EnvCI) {\n  test.skip('a', () => {});\n}", "CI or flakiness condition (EnvCI)"},
		"skipIf on a custom API":                        {"observeMeasurement.skipIf(isCI)('a', binding, async () => ({}));", "CI or flakiness condition"},
		"short circuit names CI":                        {"process.env.CI && test.skip('a', () => {});", "CI or flakiness condition (process.env.CI)"},
		"chained short circuit":                         {"isLinux && process.env.CI === 'true' && test.skip('a', () => {});", "CI or flakiness condition (isLinux&&process.env.CI==='true')"},
		"returned short circuit":                        {"function register() {\n  return isCI && test.skip('a', () => {});\n}", "CI or flakiness condition (isCI)"},
		"ternary names flakiness":                       {"isFlaky ? test.skip('a', () => {}) : test('a', () => {});", "CI or flakiness condition (isFlaky)"},
		"flaky ternary callee":                          {"(isFlaky ? test.skip : test)('a', () => {});", "CI or flakiness condition (isFlaky)"},
		"flaky ternary alias":                           {"const run = process.env.CI ? it.skip : it;", "CI or flakiness condition (process.env.CI)"},
		"skip alias":                                    {"const later = test.skip;", "`test.skip` skips unconditionally"},
		"case label names flakiness":                    {"switch (true) {\n  case isFlaky:\n    setup();\n    test.skip('a', () => {});\n}", "CI or flakiness condition (isFlaky)"},
		"bracket focus":                                 {"test['only']('a', () => {});", "`test['only']` focuses the run"},
		"bracket focus in quotes":                       {"describe[\"only\"]('a', () => {});", "`describe['only']` focuses the run"},
		"bracket focus in template":                     {"it[`only`]('a', () => {});", "focuses the run"},
		"optional bracket skip":                         {"test?.['skip']('a', () => {});", "`test?.['skip']` skips unconditionally"},
		"bracket skip modifier":                         {"test['skip']['each']([1])('a %d', () => {});", "`test['skip']` skips unconditionally"},
		"bracket after a table":                         {"describe.each([1])['only']('a', () => {});", "`describe.each([1])['only']` focuses the run"},
		"bracket skipIf names CI":                       {"it['skipIf'](isCI)('a', () => {});", "CI or flakiness condition"},
		"bracket skip names CI":                         {"if (process.env.CI) {\n  test['skip']('a', () => {});\n}", "CI or flakiness condition"},
		"nested ternary under CI":                       {"isCI ? onWin ? test.skip('a', () => {}) : 0 : 0;", "CI or flakiness condition (isCI)"},
		"nested operand under CI":                       {"isCI && (onWin && test.skip('a', () => {}));", "CI or flakiness condition (isCI)"},
		"nested braceless if under CI":                  {"if (isCI) if (onWin) test.skip('a', () => {});", "CI or flakiness condition (isCI)"},
		"describe under a CI if":                        {"if (process.env.CI) {\n  describe('a', () => {\n    test.skip('b', () => {});\n  });\n}", "CI or flakiness condition (process.env.CI)"},
		"describe after a CI operand":                   {"isCI && describe('a', () => {\n  it.skip('b', () => {});\n});", "CI or flakiness condition (isCI)"},
		"skipIf under a CI guard":                       {"if (isCI) {\n  test.skipIf(onWin)('a', () => {});\n}", "CI or flakiness condition (isCI)"},
		"optional call focus":                           {"test.only?.('a', () => {});", "`test.only` focuses the run"},
		"optional call skip":                            {"test.skip?.('a', () => {});", "`test.skip` skips unconditionally"},
		"optional bracket call focus":                   {"test?.['only']?.('a', () => {});", "`test?.['only']` focuses the run"},
		"fallthrough case label":                        {"switch (true) {\n  case isCI:\n  case onWin:\n    test.skip('a', () => {});\n}", "CI or flakiness condition (isCI onWin)"},
		"fallthrough into default":                      {"switch (true) {\n  case isCI:\n  default:\n    test.skip('a', () => {});\n}", "CI or flakiness condition (isCI)"},
		"object property value":                         {"const cases = { run: test.skip('a', () => {}) };", "`test.skip` skips unconditionally"},
		"alias as the last token":                       {"export const later = test.skip", "`test.skip` skips unconditionally"},
		"typed skip call":                               {"test.skip<Ctx>('a', () => {});", "`test.skip` skips unconditionally"},
		"typed focus call":                              {"test.only<Map<string, Array<number>>>('a', () => {});", "`test.only` focuses the run"},
		"non-null skip call":                            {"test.skip!('a', () => {});", "`test.skip` skips unconditionally"},
		"typed xit":                                     {"xit<Ctx>('a', () => {});", "`xit` skips unconditionally"},
		"loop under a CI if":                            {"if (isCI) for (const c of cases) if (onWin) test.skip(c, () => {});", "CI or flakiness condition (isCI)"},
		"assignment under a CI if":                      {"if (isCI) x = onWin && test.skip('a', () => {});", "CI or flakiness condition (isCI)"},
		"label under a CI if":                           {"if (isCI) outer: for (const c of cases) test.skip(c, () => {});", "CI or flakiness condition (isCI)"},
		"conditional break falls through":               {"switch (true) {\n  case isCI:\n    if (y) break;\n  case onWin:\n    test.skip('a', () => {});\n}", "CI or flakiness condition (isCI onWin)"},
		"grouping under a CI if":                        {"if (isCI) (onWin && test.skip('a', () => {}));", "CI or flakiness condition (isCI)"},
		"array under a CI if":                           {"if (isCI) [onWin && test.skip('a', () => {})];", "CI or flakiness condition (isCI)"},
		"new under a CI if":                             {"if (isCI) new Runner(test.skip('a', () => {}));", "CI or flakiness condition (isCI)"},
		"negation under a CI if":                        {"if (isCI) !(onWin && test.skip('a', () => {}));", "CI or flakiness condition (isCI)"},
		"try under a CI if":                             {"if (isCI) try {\n  test.skip('a', () => {});\n} finally {}", "CI or flakiness condition (isCI)"},
		"comma under a CI if":                           {"if (isCI) setup(), test.skip('a', () => {});", "CI or flakiness condition (isCI)"},
		"braceless else under a CI if":                  {"if (isCI) if (onWin) a(); else test.skip('a', () => {});", "CI or flakiness condition (isCI)"},
		"alias after a braceless if without semicolons": {"if (onWin) a = b\nconst later = test.skip", "`test.skip` skips unconditionally"},
		"return in a function ends no case":             {"switch (true) {\n  case isCI:\n    const f = () => { return 1 };\n  case 2:\n    test.skip('a', () => {});\n}", "CI or flakiness condition (isCI 2)"},
		"ternary test name":                             {"describe(isWin ? 'win' : 'posix', () => {\n  test.skip('a', () => {});\n});", "`test.skip` skips unconditionally"},
		"default before a callback":                     {"withDb(url ?? fallback, () => {\n  test.skip('a', () => {});\n});", "`test.skip` skips unconditionally"},
		"or before an argument":                         {"run(a || b, test.skip('a', () => {}));", "`test.skip` skips unconditionally"},
		"or before an array item":                       {"const runs = [a || b, test.skip('a', () => {})];", "`test.skip` skips unconditionally"},
		"comma after a CI if":                           {"if (isCI) a(), onWin && test.skip('a', () => {});", "CI or flakiness condition (isCI)"},
		"catch under a CI if":                           {"if (isCI) try {\n  a();\n} catch (e) {\n  test.skip('a', () => {});\n}", "CI or flakiness condition (isCI)"},
		"finally under a CI if":                         {"if (isCI) try {\n  a();\n} catch {\n  b();\n} finally {\n  if (onWin) test.skip('a', () => {});\n}", "CI or flakiness condition (isCI)"},
		"do under a CI if":                              {"if (isCI) do {\n  if (onWin) test.skip('a', () => {});\n} while (false);", "CI or flakiness condition (isCI)"},
		"bare catch under a CI if":                      {"if (isCI) try {\n  a();\n} catch {\n  if (onWin) test.skip('a', () => {});\n}", "CI or flakiness condition (isCI)"},
		"promise catch after a CI operand":              {"isCI && p.catch(() => {\n  if (onWin) test.skip('a', () => {});\n});", "CI or flakiness condition (isCI)"},
		"comma in a CI case":                            {"switch (x) {\n  case isCI:\n    a || b, test.skip('a', () => {});\n}", "CI or flakiness condition (isCI)"},
		"braceless do under a CI if":                    {"if (isCI) do if (onWin) test.skip('a', () => {}); while (0);", "CI or flakiness condition (isCI)"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			findings := ScanFile("a.test.ts", []byte(tc.src))
			if len(findings) != 1 {
				t.Fatalf("findings = %+v, want one", findings)
			}
			got := findings[0]
			if got.Severity != "error" || !strings.Contains(got.Message, tc.want) {
				t.Fatalf("finding = %+v, want an error containing %q", got, tc.want)
			}
		})
	}
}

func TestScanFileAllowedGuards(t *testing.T) {
	cases := map[string]string{
		"skipIf platform":                   "describe.skipIf(process.platform !== 'win32')('a', () => {});",
		"skipIf dependency":                 "describe.skipIf(!dockerAvailable)('a', () => {});",
		"skipIf binding":                    "it.skipIf(!process.env.DATABASE_URL)('a', () => {});",
		"test.if":                           "test.if(hasBinding)('a', () => {});",
		"if block":                          "if (!hasDocker) {\n  test.skip('a', () => {});\n}",
		"else block":                        "if (hasDocker) {\n  test('a', () => {});\n} else {\n  test.skip('a', () => {});\n}",
		"braceless if":                      "if (process.platform === 'win32') test.skip('a', () => {});",
		"braceless else":                    "if (a) test('a', () => {});\nelse test.skip('a', () => {});",
		"ternary":                           "hasDocker ? test('a', () => {}) : test.skip('a', () => {});",
		"ternary callee":                    "(hasDocker ? test : test.skip)('a', () => {});",
		"short circuit":                     "hasDocker || test.skip('a', () => {});",
		"switch case":                       "switch (process.platform) {\n  case 'win32':\n    test.skip('a', () => {});\n    break;\n}",
		"switch later statement":            "switch (process.platform) {\n  case 'win32':\n    setup();\n    test.skip('a', () => {});\n}",
		"guard around describe":             "if (!hasDocker) {\n  describe('a', () => {\n    test.skip('b', () => {});\n  });\n}",
		"test name names CI":                "if (!hasDocker) test.skip('parses the CI configuration', () => {});",
		"typeof skip":                       "type Skip = typeof test.skip;",
		"short circuit guard":               "!hasDocker && test.skip('a', () => {});",
		"ternary platform":                  "process.platform === 'win32' ? test.skip('a', () => {}) : test('a', () => {});",
		"else operand of CI":                "process.env.CI || test.skip('a', () => {});",
		"else branch of CI":                 "isCI ? test('a', () => {}) : test.skip('a', () => {});",
		"else block of CI":                  "if (process.env.CI) {\n  test('a', () => {});\n} else {\n  test.skip('a', () => {});\n}",
		"assigned platform skip":            "const run = process.platform === 'win32' ? test.skip : test;",
		"case label platform":               "switch (true) {\n  case isWindows:\n    setup();\n    test.skip('a', () => {});\n}",
		"default after CI case":             "switch (true) {\n  case isCI:\n    break;\n  default:\n    if (!hasDocker) test.skip('a', () => {});\n}",
		"array of member names":             "const names = ['skip', 'only'];",
		"returned member names":             "function names() {\n  return ['only'];\n}",
		"bracket property":                  "expect(summary['skip']).toBe(0);",
		"bracket builder":                   "qb['skip'](20);",
		"else operand around a guard":       "isCI || (onWin && test.skip('a', () => {}));",
		"second branch of a nested ternary": "isCI ? 0 : onWin ? test.skip('a', () => {}) : 0;",
		"case after a break":                "switch (true) {\n  case isCI:\n    break;\n  case onWin:\n    test.skip('a', () => {});\n}",
		"platform around describe":          "if (process.platform === 'win32') {\n  describe('a', () => {\n    it.skip('b', () => {});\n  });\n}",
		"ternary in an object":              "const x = { run: onWin ? test.skip('a', () => {}) : null };",
		"skip as an argument":               "expect(test.skip).toBeDefined();",
		"assigned to skip":                  "specTest.skip = wrap;",
		"deleted skip":                      "delete test.skip;",
		"keys of only":                      "Object.keys(test.only ?? {});",
		"wrapped only":                      "describe.only = wrap(describe.only);",
		"loop under a platform if":          "if (onWin) for (const c of cases) test.skip(c, () => {});",
		"typed comparison":                  "const small = page.skip < limit && total > (count);",
		"block break ends a case":           "switch (true) {\n  case isCI: {\n    setup();\n    break;\n  }\n  case onWin:\n    test.skip('a', () => {});\n}",
		"ternary item in a platform case":   "switch (x) {\n  case isWin:\n    a ? b : c, test.skip('a', () => {});\n}",
		"promise catch under a platform if": "if (onWin) p.catch(() => {\n  test.skip('a', () => {});\n});",
		"line comment":                      "// test.only('a', () => {});",
		"block comment":                     "/* describe.skip('a', () => {}); */",
		"string":                            "const s = 'test.only(\"a\")';",
		"template text":                     "const s = `test.skip('a') ${x} describe.only(`;",
		"regex":                             "const r = /test\\.only\\(/;",
		"regex with class":                  "const r = /[/].skip(/g;",
		"other member":                      "stream.skipped(3); list.onlyOne();",
		"cursor skip":                       "await db.find().skip(10).limit(5);",
		"query builder skip":                "qb.skip(20);",
		"skip property":                     "expect(summary.skip.length).toBe(0);",
		"only property":                     "expect(opts.only.includes('a'));",
		"one-argument only":                 "schema.only('a');",
		"builder if":                        "query.if(isCI);",
		"braceless return":                  "if (!hasDb) return ctx.skip('a', () => {});",
		"braceless await":                   "if (x) await test.skip('a', () => {});",
		"braceless void":                    "if (x) void test.skip('a', () => {});",
		"declared xit":                      "function xit(name, fn) {}",
		"xit method":                        "class A { xit(a, b) {} }",
		"xit property":                      "runner.xit('a', () => {});",
		"fit helper":                        "const line = fit('linear', points);",
		"skip with a paren name":            "p.skip('(', 1);",
		"typed non-arrow arg":               "p.skip('a', (x): y, 2);",
		"custom skipIf helper":              "rules.skipIf(isCI)('a', 1);",
		"guarded xit":                       "if (!hasDocker) xit('a', () => {});",
		"JSX closing tag":                   "if (!hasDom) {\n  render(<p>{a}</p>);\n  test.skip('a', () => {});\n}",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if findings := ScanFile("a.test.ts", []byte(src)); len(findings) != 0 {
				t.Fatalf("findings = %+v, want none", findings)
			}
		})
	}
}

func TestScanFilePositions(t *testing.T) {
	findings := ScanFile("a.test.ts", []byte("import { test } from 'bun:test';\n\n  test\n    .only('a', () => {});\n"))
	if len(findings) != 1 || findings[0].Line != 4 || findings[0].Column != 6 || findings[0].File != "a.test.ts" {
		t.Fatalf("findings = %+v, want one at 4:6", findings)
	}
}

func TestScanFileReviewedExceptions(t *testing.T) {
	above := ScanFile("a.test.ts", []byte("// putnami:allow-skip this case proves skip registration\nspecTest.skip('a', () => {});"))
	if len(above) != 1 || above[0].Severity != "warning" ||
		!strings.Contains(above[0].Message, "reviewed exception: `specTest.skip` stays because this case proves skip registration") {
		t.Fatalf("exception above: findings = %+v", above)
	}

	sameLine := ScanFile("a.test.ts", []byte("test.only('a', () => {}); /* putnami:allow-skip: bisecting */"))
	if len(sameLine) != 1 || sameLine[0].Severity != "warning" || !strings.Contains(sameLine[0].Message, "because bisecting") {
		t.Fatalf("exception on the same line: findings = %+v", sameLine)
	}

	aboveChain := ScanFile("a.test.ts", []byte("// putnami:allow-skip reviewed\ntest\n  .skip('a', () => {});"))
	if len(aboveChain) != 1 || aboveChain[0].Severity != "warning" {
		t.Fatalf("exception above a multi-line chain: findings = %+v", aboveChain)
	}

	noReason := ScanFile("a.test.ts", []byte("// putnami:allow-skip\ntest.skip('a', () => {});"))
	if len(noReason) != 1 || noReason[0].Severity != "error" || !strings.Contains(noReason[0].Message, "needs a reason") {
		t.Fatalf("exception without a reason: findings = %+v", noReason)
	}

	other := ScanFile("a.test.ts", []byte("// putnami:allow-skipping later\ntest.skip('a', () => {});"))
	if len(other) != 1 || other[0].Severity != "error" {
		t.Fatalf("another directive is not an exception: findings = %+v", other)
	}

	farAbove := ScanFile("a.test.ts", []byte("// putnami:allow-skip too far\n\ntest.skip('a', () => {});"))
	if len(farAbove) != 1 || farAbove[0].Severity != "error" {
		t.Fatalf("an exception two lines above does not apply: findings = %+v", farAbove)
	}

	trailing := ScanFile("a.test.ts", []byte("test.skip('a', () => {}); // putnami:allow-skip reviewed\ntest.skip('b', () => {});"))
	if len(trailing) != 2 || trailing[0].Severity != "warning" || trailing[1].Severity != "error" {
		t.Fatalf("a trailing exception covers its own line only: findings = %+v", trailing)
	}

	blockAbove := ScanFile("a.test.ts", []byte("  /* putnami:allow-skip reviewed */\ntest.skip('a', () => {});"))
	if len(blockAbove) != 1 || blockAbove[0].Severity != "warning" {
		t.Fatalf("a standalone block comment above applies: findings = %+v", blockAbove)
	}
}

func TestScanFileSurvivesUnterminatedLiterals(t *testing.T) {
	for _, src := range []string{
		"const s = 'open\ntest.only('a');",
		"const s = `open",
		"/* open",
		"const r = /open\ntest.only('a');",
		"}}}) test.only('a');",
		"test.skipIf(",
		"describe.if(isLinux",
		"test.skipIf(x === 'a)",
		"xit(",
		"test.skip(",
	} {
		ScanFile("a.test.ts", []byte(src))
	}
	if findings := ScanFile("a.test.ts", []byte("const s = 'open\ntest.only('a');")); len(findings) != 1 {
		t.Fatalf("a string ends at its line: findings = %+v, want one", findings)
	}
}

func TestScanProject(t *testing.T) {
	root := t.TempDir()
	focused := []byte("test.only('a', () => {});\n")
	write := func(rel string, content []byte) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("putnami.json", []byte("{}"))
	write("test/a.test.ts", focused)
	write("test/b.spec.tsx", focused)
	write("src/c_test.js", focused)
	write("src/d.test.mts", focused)
	write("src/code.ts", focused)
	write("src/types.test.d.ts", focused)
	write("node_modules/x/a.test.ts", focused)
	write("dist/a.test.js", focused)
	write(".gen/a.test.ts", focused)
	write("nested/putnami.json", []byte("{}"))
	write("nested/a.test.ts", focused)

	findings, err := ScanProject(root)
	if err != nil {
		t.Fatal(err)
	}
	files := make([]string, 0, len(findings))
	for _, finding := range findings {
		rel, _ := filepath.Rel(root, finding.File)
		files = append(files, filepath.ToSlash(rel))
	}
	if got := strings.Join(files, ","); got != "src/c_test.js,src/d.test.mts,test/a.test.ts,test/b.spec.tsx" {
		t.Fatalf("scanned files = %s", got)
	}

	if _, err := ScanProject(filepath.Join(root, "missing")); err == nil {
		t.Fatal("a missing project root must be an error")
	}
}

func TestScanProjectSkipsDirectoriesGitIgnores(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	focused := []byte("test.only('a', () => {});\n")
	for rel, content := range map[string][]byte{
		".gitignore":      []byte("build/\n"),
		"putnami.json":    []byte("{}"),
		"a.test.ts":       focused,
		"build/a.test.js": focused,
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}

	findings, err := ScanProject(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || filepath.Base(findings[0].File) != "a.test.ts" {
		t.Fatalf("findings = %+v, want a.test.ts only", findings)
	}
}

func TestScanFileReportsEveryFormInOneFile(t *testing.T) {
	src := "import { describe, test } from 'bun:test';\n\n" +
		"test.only('focused probe', () => {});\n" +
		"test.skip('unguarded probe', () => {});\n" +
		"describe.skipIf(process.platform === 'plan9')('platform probe', () => {});\n" +
		"// putnami:allow-skip the acceptance probe proves the exception path\n" +
		"test.skip('reviewed probe', () => {});\n"
	findings := ScanFile("a.test.ts", []byte(src))
	got := make([]string, 0, len(findings))
	for _, finding := range findings {
		got = append(got, strconv.Itoa(finding.Line)+":"+finding.Severity)
	}
	if strings.Join(got, ",") != "3:error,4:error,7:warning" {
		t.Fatalf("findings = %v (%+v), want 3:error,4:error,7:warning", got, findings)
	}
}
