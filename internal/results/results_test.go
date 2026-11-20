package results

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sample() *Results {
	return &Results{
		Version: SchemaVersion,
		Run:     RunInfo{Suite: "S"},
		Scenarios: []ScenarioResult{
			{ID: "setup", Kind: "setup", Status: Passed, Steps: []StepResult{{ID: "c", Status: Passed, Groups: []GroupResult{{ID: "setup/c/x", Name: "setup check", Status: Passed}}}}},
			{ID: "a", Kind: "regression", Status: Failed, Steps: []StepResult{
				{ID: "s1", Status: Failed, Groups: []GroupResult{
					{ID: "a/s1/p", Name: "passes", Status: Passed},
					{ID: "a/s1/f", Name: "fails", Status: Failed, Checks: []CheckResult{{Desc: "json /x eq 1", Status: Failed, Message: "expected 1, got 2"}}},
					{ID: "a/s1/e", Name: "errors", Status: Errored, Checks: []CheckResult{{Desc: "json /y", Status: Errored, Message: "body is not JSON"}}},
				}},
				{ID: "s2", Status: Blocked, Reason: "blocked: step s1 did not pass", Groups: []GroupResult{{ID: "a/s2/b", Name: "blocked one", Status: Blocked}}},
			}},
			{ID: "b", Kind: "regression", Status: Blocked, Reason: "dependency a did not pass (failed)", Steps: []StepResult{{ID: "s", Status: Blocked, Groups: []GroupResult{{ID: "b/s/g", Name: "also blocked", Status: Blocked}}}}},
		},
	}
}

func TestTotalsAreDerivedAndSeparateSetup(t *testing.T) {
	r := sample()
	r.Compute()
	a := r.Totals.Assertions
	if a.Total != 5 || a.Passed != 1 || a.Failed != 1 || a.Errored != 1 || a.Blocked != 2 {
		t.Fatalf("assertions: %+v", a)
	}
	if a.Total != a.Passed+a.Failed+a.Errored+a.Blocked {
		t.Fatalf("total must equal the sum of outcomes: %+v", a)
	}
	if r.Totals.SetupAssertions.Total != 1 {
		t.Fatalf("setup checks are counted separately: %+v", r.Totals)
	}
	if r.Totals.Scenarios.Total != 2 || r.Totals.Scenarios.Failed != 1 || r.Totals.Scenarios.Blocked != 1 {
		t.Fatalf("scenarios exclude setup: %+v", r.Totals.Scenarios)
	}
	if r.AllPassed() {
		t.Fatal("not all passed")
	}
}

func TestJUnitDistinguishesFailureErrorAndSkipped(t *testing.T) {
	data, err := JUnit(sample())
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Failures int `xml:"failures,attr"`
		Errors   int `xml:"errors,attr"`
		Skipped  int `xml:"skipped,attr"`
		Tests    int `xml:"tests,attr"`
	}
	if err := xml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("junit must be well-formed: %v", err)
	}
	if parsed.Tests != 6 || parsed.Failures != 1 || parsed.Errors != 1 || parsed.Skipped != 2 {
		t.Fatalf("%+v\n%s", parsed, data)
	}
	if !strings.Contains(string(data), "expected 1, got 2") {
		t.Error("failure text should carry the check message")
	}
}

func TestPrepareOutputDirRemovesOnlyThePreviousRunsOwnFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	if err := PrepareOutputDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := WriteAll(dir, sample()); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"results.json", "junit.xml"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("%s not written: %v", f, err)
		}
	}
	// things the runner owns (including a report and the old attachments folder) and things it does not
	os.WriteFile(filepath.Join(dir, "report.html"), []byte("old report"), 0o644)
	os.MkdirAll(filepath.Join(dir, "attachments"), 0o755)
	os.WriteFile(filepath.Join(dir, "attachments", "0001-res.txt"), []byte("old"), 0o644)
	os.WriteFile(filepath.Join(dir, "my-notes.txt"), []byte("keep me"), 0o644)
	os.MkdirAll(filepath.Join(dir, "other"), 0o755)

	if err := PrepareOutputDir(dir); err != nil {
		t.Fatalf("an output directory with a previous run must simply be reused: %v", err)
	}
	for _, gone := range []string{"results.json", "junit.xml", "report.html", "attachments"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); err == nil {
			t.Errorf("%s from the previous run must be removed", gone)
		}
	}
	for _, kept := range []string{"my-notes.txt", "other"} {
		if _, err := os.Stat(filepath.Join(dir, kept)); err != nil {
			t.Errorf("%s is not the runner's and must be left alone", kept)
		}
	}
	if err := PrepareOutputDir(filepath.Join(dir, "new", "nested")); err != nil {
		t.Fatalf("a missing directory is created: %v", err)
	}
}

func TestHelperAndCleanupChecksAreNotCountedAsRegressionAssertions(t *testing.T) {
	r := &Results{Scenarios: []ScenarioResult{{ID: "a", Kind: "regression", Status: Passed, Steps: []StepResult{
		{ID: "main", Status: Passed, Groups: []GroupResult{{ID: "a/main/x", Name: "x", Status: Passed}}},
		{ID: "main-helper", Helper: true, Status: Passed, Groups: []GroupResult{{ID: "a/main-helper/y", Name: "helper check", Status: Passed}}},
		{ID: "close", Cleanup: true, Status: Passed, Groups: []GroupResult{{ID: "a/close/z", Name: "cleanup check", Status: Passed}}},
	}}}}
	r.Compute()
	if r.Totals.Assertions.Total != 1 || r.Totals.SetupAssertions.Total != 2 {
		t.Fatalf("helper/cleanup checks must not inflate the regression count: %+v", r.Totals)
	}
}
