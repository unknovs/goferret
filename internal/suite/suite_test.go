package suite

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const inputs = `version: 1
inputs:
  BASE: {type: url, required: true}
`

const head = `version: 1
name: T
inputs: inputs.yaml
fixtureRoot: fx
tags: [a]
services:
  api: { baseUrl: "{{BASE}}" }
scenarios: [s.yaml]
`

func write(t *testing.T, suiteYAML, scen string) string {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "fx"), 0o755)
	for n, b := range map[string]string{"inputs.yaml": inputs, "suite.yaml": suiteYAML, "s.yaml": scen} {
		os.WriteFile(filepath.Join(dir, n), []byte(b), 0o644)
	}
	return filepath.Join(dir, "suite.yaml")
}

func problems(t *testing.T, suiteYAML, scen string) string {
	t.Helper()
	s, err := Load(write(t, suiteYAML, scen))
	if err != nil {
		return "LOAD: " + err.Error()
	}
	var parts []string
	for _, e := range s.Validate() {
		parts = append(parts, e.Error())
	}
	return strings.Join(parts, "\n")
}

func step(extra string) string {
	return "version: 1\nscenarios:\n  - id: one\n    tags: [a]\n    steps:\n      - id: s\n        service: api\n        method: GET\n        path: /x\n" + extra
}

func TestValidSuite(t *testing.T) {
	if p := problems(t, head, step("")); p != "" {
		t.Fatalf("expected valid: %s", p)
	}
}

func TestStrictUnknownFields(t *testing.T) {
	if p := problems(t, head, step("        nope: 1\n")); !strings.HasPrefix(p, "LOAD:") || !strings.Contains(p, "nope") {
		t.Fatalf("unknown field must fail loading with context: %s", p)
	}
	if p := problems(t, head+"extra: 1\n", step("")); !strings.HasPrefix(p, "LOAD:") {
		t.Fatalf("unknown suite field: %s", p)
	}
}

func TestValidationProblems(t *testing.T) {
	cases := map[string]struct{ scen, want string }{
		"unknown variable": {step("        query: { k: \"{{nope}}\" }\n"), "unknown variable {{nope}}"},
		"unknown service":  {strings.Replace(step(""), "service: api", "service: zzz", 1), "unknown service"},
		"unknown tag":      {strings.Replace(step(""), "[a]", "[zzz]", 1), "not declared"},
		"bad method":       {strings.Replace(step(""), "GET", "FETCH", 1), "unsupported method"},
		"no operator":      {step("        assert: [ { name: n, checks: [ { json: /a } ] } ]\n"), "no operator"},
		"two targets":      {step("        assert: [ { name: n, checks: [ { json: /a, header: H, exists: true } ] } ]\n"), "exactly one target"},
		"bad pointer":      {step("        assert: [ { name: n, checks: [ { json: a, exists: true } ] } ]\n"), "must be empty or start"},
		"bad regex":        {step("        assert: [ { name: n, checks: [ { json: /a, matches: '(' } ] } ]\n"), "invalid regular expression"},
		"bad type":         {step("        assert: [ { name: n, checks: [ { json: /a, type: text } ] } ]\n"), "unknown type"},
		"dup group":        {step("        assert: [ { name: n, checks: [ { status: 200 } ] }, { name: n, checks: [ { status: 200 } ] } ]\n"), "duplicate assertion group"},
		"capture two":      {step("        capture: { v: { json: /a, header: H } }\n"), "exactly one of"},
		"poll on post":     {strings.Replace(step("        poll: { attempts: 2, intervalSeconds: 1, until: [ { status: 200 } ] }\n"), "GET", "POST", 1), "only allowed on GET"},
		"fixture escape":   {step("        body: \"{{$file_text ../secret.txt}}\"\n"), "escapes the fixture root"},
		"json and body":    {step("        json: {a: 1}\n        body: x\n"), "mutually exclusive"},
		"missing export":   {strings.Replace(step(""), "    steps:", "    exports: [ghost]\n    steps:", 1), "never defined"},
	}
	for name, tc := range cases {
		if p := problems(t, head, tc.scen); !strings.Contains(p, tc.want) {
			t.Errorf("%s: want %q in:\n%s", name, tc.want, p)
		}
	}
}

func TestDuplicateIDsCyclesAndDependencies(t *testing.T) {
	s := "version: 1\nscenarios:\n"
	one := func(id, deps string) string {
		return "  - id: " + id + "\n    dependsOn: [" + deps + "]\n    steps: [ { id: s, service: api, method: GET, path: /x } ]\n"
	}
	if p := problems(t, head, s+one("a", "")+one("a", "")); !strings.Contains(p, "duplicate scenario id") {
		t.Errorf("duplicate: %s", p)
	}
	if p := problems(t, head, s+one("a", "b")+one("b", "a")); !strings.Contains(p, "dependency cycle") {
		t.Errorf("cycle: %s", p)
	}
	if p := problems(t, head, s+one("a", "zzz")); !strings.Contains(p, "unknown scenario") {
		t.Errorf("unknown dep: %s", p)
	}
}

func TestVariableVisibilityFollowsOrderAndExports(t *testing.T) {
	scen := `version: 1
scenarios:
  - id: p
    exports: [made]
    steps:
      - { id: one, service: api, method: GET, path: "/a/{{late}}" }
      - { id: two, service: api, method: GET, path: /b, capture: { late: { json: /x }, made: { json: /y } } }
  - id: q
    dependsOn: [p]
    steps:
      - { id: s, service: api, method: GET, path: "/c/{{made}}" }
  - id: r
    steps:
      - { id: s, service: api, method: GET, path: "/c/{{made}}" }
`
	p := problems(t, head, scen)
	if !strings.Contains(p, "{{late}}") {
		t.Errorf("a capture is not visible before the step that sets it: %s", p)
	}
	if strings.Count(p, "{{made}}") != 1 {
		t.Errorf("an export is visible to dependents only (exactly one error expected): %s", p)
	}
}

func TestMatrixExpansion(t *testing.T) {
	scen := `version: 1
scenarios:
  - id: m
    matrix: [ { suffix: der, method: DER }, { suffix: p1363, method: P1363 } ]
    steps: [ { id: s, service: api, method: GET, path: "/x/{{method}}" } ]
`
	s, err := Load(write(t, head, scen))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Loaded) != 2 || s.Loaded[0].ID != "m.der" || s.Loaded[1].ID != "m.p1363" || s.Loaded[1].MatrixVars["method"] != "P1363" {
		t.Fatalf("%+v", s.Loaded)
	}
	if errs := s.Validate(); len(errs) != 0 {
		t.Fatal(errs)
	}
	if _, err := Load(write(t, head, strings.Replace(scen, "suffix: der, ", "", 1))); err == nil {
		t.Error("a matrix entry without suffix must fail")
	}
}

func TestFixturePathCannotEscape(t *testing.T) {
	s, err := Load(write(t, head, step("")))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../x", "a/../../x", "/etc/passwd"} {
		if _, err := s.FixturePath(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
	if p, err := s.FixturePath("a/b.txt"); err != nil || !strings.HasSuffix(filepath.ToSlash(p), "fx/a/b.txt") {
		t.Errorf("%v %v", p, err)
	}
}
