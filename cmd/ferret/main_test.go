package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

const inputsYAML = `version: 1
inputs:
  BASE: {type: url, required: true}
  KEY:  {type: string, required: true, secret: true}
`

const suiteYAML = `version: 1
name: CLI
inputs: inputs.yaml
tags: [fast, other]
services:
  api: { baseUrl: "{{BASE}}", headers: { APIKey: "{{KEY}}" } }
scenarios: [s.yaml]
`

func scenarios(expect int) string {
	return `version: 1
scenarios:
  - id: one
    tags: [fast]
    steps:
      - id: s
        service: api
        method: GET
        path: /x
        assert: [ { name: status, checks: [ { status: ` + strconv.Itoa(expect) + ` } ] } ]
  - id: two
    tags: [other]
    steps:
      - id: s
        service: api
        method: GET
        path: /x
        assert: [ { name: status, checks: [ { status: 200 } ] } ]
`
}

func setup(t *testing.T, expect int) (suitePath, base string, hits *atomic.Int32) {
	t.Helper()
	hits = &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	for n, b := range map[string]string{"inputs.yaml": inputsYAML, "suite.yaml": suiteYAML, "s.yaml": scenarios(expect)} {
		os.WriteFile(filepath.Join(dir, n), []byte(b), 0o644)
	}
	t.Setenv("TESTS", "")
	return filepath.Join(dir, "suite.yaml"), srv.URL, hits
}

func exec(t *testing.T, args ...string) (code int, out, errOut string) {
	t.Helper()
	var o, e bytes.Buffer
	code = realMain(args, &o, &e)
	return code, o.String(), e.String()
}

func TestExitCodes(t *testing.T) {
	suitePath, base, hits := setup(t, 200)
	t.Setenv("BASE", base)
	t.Setenv("KEY", "cli-secret-key-1")

	out := filepath.Join(t.TempDir(), "ok")
	if code, so, se := exec(t, "run", "--tests", suitePath, "--output", out); code != 0 {
		t.Fatalf("all passing must exit 0, got %d\n%s\n%s", code, so, se)
	}
	for _, f := range []string{"results.json", "junit.xml"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("%s missing", f)
		}
	}
	data, _ := os.ReadFile(filepath.Join(out, "results.json"))
	if strings.Contains(string(data), "cli-secret-key-1") {
		t.Error("secret leaked into results.json")
	}

	// the same output directory can be reused: the previous run's files are replaced
	first, _ := os.ReadFile(filepath.Join(out, "results.json"))
	os.WriteFile(filepath.Join(out, "my-notes.txt"), []byte("keep me"), 0o644)
	before := hits.Load()
	if code, _, se := exec(t, "run", "--tests", suitePath, "--output", out); code != 0 {
		t.Fatalf("rerunning into the same output dir must work, got %d: %s", code, se)
	}
	if hits.Load() == before {
		t.Error("the second run must actually run")
	}
	second, _ := os.ReadFile(filepath.Join(out, "results.json"))
	if string(first) == string(second) {
		t.Error("results.json must be the new run's")
	}
	if _, err := os.Stat(filepath.Join(out, "my-notes.txt")); err != nil {
		t.Error("files the runner does not own must be left alone")
	}

	// a run that fails before writing results leaves no stale results.json behind
	t.Setenv("KEY", "")
	os.Unsetenv("KEY")
	if code, _, _ := exec(t, "run", "--tests", suitePath, "--output", out); code != 2 {
		t.Fatalf("missing configuration must exit 2")
	}
	if _, err := os.Stat(filepath.Join(out, "results.json")); err == nil {
		t.Error("a stale results.json from the previous run must not survive a run that never started")
	}
}

func TestFailingAssertionExitsOneAndPartialRunIsLabelled(t *testing.T) {
	suitePath, base, _ := setup(t, 404)
	t.Setenv("BASE", base)
	t.Setenv("KEY", "cli-secret-key-1")
	out := filepath.Join(t.TempDir(), "r")
	code, so, _ := exec(t, "run", "--tests", suitePath, "--output", out)
	if code != 1 || !strings.Contains(so, "FAIL") || !strings.Contains(so, "failed 1") {
		t.Fatalf("code=%d\n%s", code, so)
	}

	out2 := filepath.Join(t.TempDir(), "r2")
	code, so, _ = exec(t, "run", "--tests", suitePath, "--tag", "other", "--output", out2)
	if code != 0 || !strings.Contains(so, "Partial run: 1 of 2") {
		t.Fatalf("a filtered run that passes exits 0 but must say it is partial: code=%d\n%s", code, so)
	}
}

func TestPreflightSendsNothingWhenConfigurationIsMissing(t *testing.T) {
	suitePath, base, hits := setup(t, 200)
	t.Setenv("BASE", base)
	t.Setenv("KEY", "")
	os.Unsetenv("KEY")
	code, _, se := exec(t, "run", "--tests", suitePath, "--output", filepath.Join(t.TempDir(), "o"))
	if code != 2 || !strings.Contains(se, "required input KEY") || hits.Load() != 0 {
		t.Fatalf("code=%d hits=%d\n%s", code, hits.Load(), se)
	}
}

func TestUsageAndDefinitionErrorsExitTwo(t *testing.T) {
	suitePath, _, _ := setup(t, 200)
	if code, _, se := exec(t, "run", "--output", "x"); code != 2 || !strings.Contains(se, "no suite given") {
		t.Errorf("missing --tests: %d %s", code, se)
	}
	if code, _, _ := exec(t); code != 2 {
		t.Error("no command")
	}
	if code, _, _ := exec(t, "bogus"); code != 2 {
		t.Error("unknown command")
	}
	if code, _, se := exec(t, "run", "--tests", suitePath); code != 2 || !strings.Contains(se, "--output is required") {
		t.Errorf("missing --output: %d %s", code, se)
	}
	if code, _, se := exec(t, "run", "--tests", suitePath, "--output", filepath.Join(t.TempDir(), "o"), "--tag", "nope"); code != 2 || !strings.Contains(se, "unknown tag") {
		t.Errorf("unknown tag: %d %s", code, se)
	}
	bad := filepath.Join(filepath.Dir(suitePath), "s.yaml")
	os.WriteFile(bad, []byte("version: 1\nscenarios:\n  - id: x\n    steps: [ { id: s, service: nope, method: GET, path: /x } ]\n"), 0o644)
	if code, _, se := exec(t, "validate", "--tests", suitePath); code != 2 || !strings.Contains(se, "unknown service") {
		t.Errorf("invalid suite: %d %s", code, se)
	}
}

func TestTestsEnvVariableAndFlagPrecedence(t *testing.T) {
	suitePath, base, _ := setup(t, 200)
	t.Setenv("BASE", base)
	t.Setenv("KEY", "cli-secret-key-1")
	t.Setenv("TESTS", suitePath)
	if code, _, se := exec(t, "validate"); code != 0 {
		t.Fatalf("TESTS env must be used: %d %s", code, se)
	}
	if code, _, _ := exec(t, "validate", "--tests", filepath.Join(t.TempDir(), "missing.yaml")); code != 2 {
		t.Fatal("--tests must win over TESTS")
	}
}

func TestEnvFileAndProcessEnvironmentPrecedence(t *testing.T) {
	suitePath, base, hits := setup(t, 200)
	envFile := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(envFile, []byte("BASE=http://127.0.0.1:1\nKEY=from-file-key-1\n"), 0o644)
	t.Setenv("BASE", base) // environment overrides the file
	code, so, se := exec(t, "run", "--tests", suitePath, "--env-file", envFile, "--output", filepath.Join(t.TempDir(), "o"))
	if code != 0 || hits.Load() == 0 {
		t.Fatalf("process env BASE must override the env file: %d\n%s\n%s", code, so, se)
	}
	os.WriteFile(envFile, []byte("BASE=x\nUNKNOWN=1\n"), 0o644)
	if code, _, se := exec(t, "validate", "--tests", suitePath, "--env-file", envFile); code != 2 || !strings.Contains(se, "UNKNOWN") {
		t.Fatalf("unknown env file key: %d %s", code, se)
	}
}

func TestListShowsTagsAndRequiredInputs(t *testing.T) {
	suitePath, _, _ := setup(t, 200)
	code, so, _ := exec(t, "list", "--tests", suitePath, "--tag", "fast")
	if code != 0 || !strings.Contains(so, "one") || strings.Contains(so, "two ") || !strings.Contains(so, "required inputs: BASE, KEY") {
		t.Fatalf("%d\n%s", code, so)
	}
}

func TestDocsCommandUpdatesAndChecksReadme(t *testing.T) {
	suitePath, _, _ := setup(t, 200)
	readme := filepath.Join(filepath.Dir(suitePath), "README.md")
	os.WriteFile(readme, []byte("# T\n<!-- tests:begin (generated by `ferret docs`; do not edit) -->\n<!-- tests:end -->\n"), 0o644)

	if code, _, se := exec(t, "docs", "--tests", suitePath, "--check"); code != 1 || !strings.Contains(se, "out of date") {
		t.Fatalf("a stale README must fail --check with exit 1: %d %s", code, se)
	}
	if code, so, se := exec(t, "docs", "--tests", suitePath); code != 0 || !strings.Contains(so, "updated") {
		t.Fatalf("%d %s %s", code, so, se)
	}
	data, _ := os.ReadFile(readme)
	if !strings.Contains(string(data), "<code>one</code>") || !strings.Contains(string(data), "| `BASE` | url | yes | no |") {
		t.Fatalf("generated content missing:\n%s", data)
	}
	if code, so, _ := exec(t, "docs", "--tests", suitePath, "--check"); code != 0 || !strings.Contains(so, "up to date") {
		t.Fatalf("a fresh README passes --check: %d %s", code, so)
	}
	os.WriteFile(readme, []byte("no markers"), 0o644)
	if code, _, se := exec(t, "docs", "--tests", suitePath); code != 2 || !strings.Contains(se, "markers") {
		t.Fatalf("missing markers is a usage error: %d %s", code, se)
	}
}

func TestSeveralEnvFilesLaterOverridesEarlier(t *testing.T) {
	suitePath, base, hits := setup(t, 200)
	dir := t.TempDir()
	shared := filepath.Join(dir, "shared.env")
	local := filepath.Join(dir, "local.env")
	os.WriteFile(shared, []byte("BASE=http://127.0.0.1:1\nKEY=shared-key-1\n"), 0o644)
	os.WriteFile(local, []byte("BASE="+base+"\n"), 0o644) // secrets stay in the first file
	code, so, se := exec(t, "run", "--tests", suitePath, "--env-file", shared, "--env-file", local, "--output", filepath.Join(t.TempDir(), "o"))
	if code != 0 || hits.Load() == 0 {
		t.Fatalf("the later file must override BASE: %d\n%s\n%s", code, so, se)
	}
	// X_FILE in a later file replaces an earlier X
	keyFile := filepath.Join(dir, "key.txt")
	os.WriteFile(keyFile, []byte("from-file-key"), 0o644)
	os.WriteFile(local, []byte("BASE="+base+"\nKEY_FILE="+keyFile+"\n"), 0o644)
	if code, so, se := exec(t, "run", "--tests", suitePath, "--env-file", shared, "--env-file", local, "--output", filepath.Join(t.TempDir(), "o2")); code != 0 {
		t.Fatalf("later KEY_FILE must replace earlier KEY: %d\n%s\n%s", code, so, se)
	}
}

func TestRerunFailedRunsOnlyScenariosThatDidNotPass(t *testing.T) {
	suitePath, base, _ := setup(t, 404) // scenario "one" expects 404 and fails, "two" passes
	t.Setenv("BASE", base)
	t.Setenv("KEY", "cli-secret-key-1")
	first := filepath.Join(t.TempDir(), "first")
	if code, _, _ := exec(t, "run", "--tests", suitePath, "--output", first); code != 1 {
		t.Fatal("setup run must fail")
	}
	second := filepath.Join(t.TempDir(), "second")
	code, so, _ := exec(t, "run", "--tests", suitePath, "--rerun-failed", filepath.Join(first, "results.json"), "--output", second)
	if code != 1 || !strings.Contains(so, "rerunning 1 scenario(s)") || !strings.Contains(so, "one") || strings.Contains(so, "two ") || !strings.Contains(so, "Partial run: 1 of 2") {
		t.Fatalf("code=%d\n%s", code, so)
	}

	// nothing to rerun is a usage error, and a missing or unsupported file is too
	suitePath2, base2, _ := setup(t, 200)
	t.Setenv("BASE", base2)
	allPass := filepath.Join(t.TempDir(), "pass")
	exec(t, "run", "--tests", suitePath2, "--output", allPass)
	if code, _, se := exec(t, "run", "--tests", suitePath2, "--rerun-failed", filepath.Join(allPass, "results.json"), "--output", filepath.Join(t.TempDir(), "x")); code != 2 || !strings.Contains(se, "no failed") {
		t.Fatalf("%d %s", code, se)
	}
	if code, _, _ := exec(t, "run", "--tests", suitePath2, "--rerun-failed", filepath.Join(t.TempDir(), "missing.json"), "--output", filepath.Join(t.TempDir(), "y")); code != 2 {
		t.Fatal("a missing results file must exit 2")
	}
}

func TestDebugPrintsTheScenarioLayoutToTheConsole(t *testing.T) {
	suitePath, base, _ := setup(t, 404) // scenario "one" fails (expects 404, gets 200), "two" is not selected
	t.Setenv("BASE", base)
	t.Setenv("KEY", "cli-secret-key-1")
	out := filepath.Join(t.TempDir(), "dbg")
	code, so, se := exec(t, "run", "--tests", suitePath, "--scenario", "one", "--debug", "--output", out)
	if code != 1 {
		t.Fatalf("code %d", code)
	}
	if strings.Contains(se, "---one----") {
		t.Error("the log goes to the console (stdout), not to stderr")
	}
	order := []string{
		"---one----",             // scenario header
		"----step.s  GET /x",     // step header
		"call #1 GET ",           // its call
		"call #1 request body (", // the step did not pass, so its bodies are shown
		"call #1 response body (",
		"-----Assertions", // numbered assertions
		"  1  [s] status  FAIL",
		"- status == 404: expected status 404, got 200",
		"FAIL    one ", // the normal one-line summary follows the block
	}
	pos := 0
	for _, want := range order {
		i := strings.Index(so[pos:], want)
		if i < 0 {
			t.Fatalf("missing or out of order: %q in\n%s", want, so)
		}
		pos += i + len(want)
	}
	if strings.Contains(so, "two") && strings.Contains(so, "---two") {
		t.Error("a scenario that was not selected must not appear in the log")
	}
	if strings.Contains(so, "cli-secret-key-1") || strings.Contains(se, "cli-secret-key-1") {
		t.Error("the log must never contain a secret")
	}
	if _, err := os.Stat(filepath.Join(out, "debug.log")); err == nil {
		t.Error("no log file may be written unless --log-file is given")
	}
}

func TestPassingStepsShowCallsButNoBodiesAndNothingIsPrintedWithoutTheFlag(t *testing.T) {
	suitePath, base, _ := setup(t, 200)
	t.Setenv("BASE", base)
	t.Setenv("KEY", "cli-secret-key-1")
	_, quiet, _ := exec(t, "run", "--tests", suitePath, "--output", filepath.Join(t.TempDir(), "q"))
	if strings.Contains(quiet, "----step.") || strings.Contains(quiet, "call #") || strings.Contains(quiet, "-----Assertions") {
		t.Fatalf("without --debug the output is the short summary only:\n%s", quiet)
	}
	_, loud, _ := exec(t, "run", "--tests", suitePath, "--debug", "--output", filepath.Join(t.TempDir(), "l"))
	for _, want := range []string{"---one----", "----step.s  GET /x", "call #1 GET ", "-> 200", "  1  [s] status  PASS"} {
		if !strings.Contains(loud, want) {
			t.Errorf("missing %q in\n%s", want, loud)
		}
	}
	if strings.Contains(loud, "response body (") || strings.Contains(loud, "request body (") {
		t.Error("bodies are shown only for a step that did not pass")
	}
}

func TestLogFileIsWrittenOnlyWhenAskedAndHoldsTheSameText(t *testing.T) {
	suitePath, base, _ := setup(t, 404)
	t.Setenv("BASE", base)
	t.Setenv("KEY", "cli-secret-key-1")
	dir := t.TempDir()
	logPath := filepath.Join(dir, "run.log")
	out := filepath.Join(dir, "o")
	_, so, _ := exec(t, "run", "--tests", suitePath, "--scenario", "one", "--log-file", logPath, "--output", out)
	if strings.Contains(so, "----step.") {
		t.Error("--log-file alone must not print the log to the console")
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("the log file must exist when requested: %v", err)
	}
	for _, want := range []string{"---one----", "----step.s  GET /x", "-----Assertions", "FAIL"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("log file is missing %q", want)
		}
	}
	if strings.Contains(string(data), "cli-secret-key-1") {
		t.Error("the log file must be redacted")
	}
	if _, err := os.Stat(filepath.Join(out, "debug.log")); err == nil {
		t.Error("no automatic debug.log")
	}
	// both together: console and file carry the same lines
	logPath2 := filepath.Join(dir, "both.log")
	_, so2, _ := exec(t, "run", "--tests", suitePath, "--scenario", "one", "--debug", "--log-file", logPath2, "--output", filepath.Join(dir, "o2"))
	both, _ := os.ReadFile(logPath2)
	if !strings.Contains(so2, "---one----") || !strings.Contains(string(both), "---one----") {
		t.Error("--debug with --log-file writes to both")
	}
	if code, _, se := exec(t, "run", "--tests", suitePath, "--log-file", filepath.Join(dir, "no", "such", "dir", "x.log"), "--output", filepath.Join(dir, "o3")); code != 2 || !strings.Contains(se, "log file") {
		t.Errorf("an unwritable log file is a usage error: %d %s", code, se)
	}
}
