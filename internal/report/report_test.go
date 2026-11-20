package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unknovs/goferret/internal/results"
)

const evil = `</script><script>window.pwned=1</script><img src=x onerror=alert(1)> & "quotes" \u2028`

func sample() *results.Results {
	r := &results.Results{
		Version: results.SchemaVersion, Tool: "ferret",
		Run: results.RunInfo{Suite: "S " + evil, Selection: results.Selection{Complete: true, Available: 1, Selected: 1}},
		Scenarios: []results.ScenarioResult{{
			ID: "a", Kind: "regression", Status: results.Failed, Description: evil,
			Steps: []results.StepResult{{ID: "s", Status: results.Failed, Reason: evil, CallSeqs: []int{1}, Groups: []results.GroupResult{{
				ID: "a/s/x", Name: evil, Status: results.Failed, Checks: []results.CheckResult{{Desc: "json /x", Status: results.Failed, Message: evil}},
			}}}},
		}},
		Calls: []results.Call{{Seq: 1, Scenario: "a", Step: "s", Method: "GET", URL: "http://x/" + evil,
			ResBody: results.Body{Bytes: 99999, Preview: evil, Truncated: true, Attachment: "attachments/0001-res.txt"}}},
	}
	r.Compute()
	return r
}

func write(t *testing.T, dir string, r *results.Results) string {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	data, _ := json.MarshalIndent(r, "", " ")
	p := filepath.Join(dir, "results.json")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBuildEmbedsDataAndNeverLetsItCloseTheScript(t *testing.T) {
	dir := t.TempDir()
	in := write(t, dir, sample())
	os.MkdirAll(filepath.Join(dir, "attachments"), 0o755)
	os.WriteFile(filepath.Join(dir, "attachments", "0001-res.txt"), []byte("full body"), 0o644)
	out := filepath.Join(dir, "report.html")
	res, err := Build(in, out)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Missing) != 0 || res.Attachments != 1 {
		t.Fatalf("%+v", res)
	}
	page, _ := os.ReadFile(out)
	s := string(page)
	for _, bad := range []string{"</script><script>window.pwned", "<img src=x onerror", "<script>window"} {
		if strings.Contains(s, bad) {
			t.Fatalf("payload %q reached the page unescaped", bad)
		}
	}
	if strings.Count(s, "<script") != 3 || strings.Count(s, "</script>") != 3 {
		t.Fatalf("exactly the 3 template scripts may exist, got %d opens / %d closes", strings.Count(s, "<script"), strings.Count(s, "</script>"))
	}
	for _, want := range []string{`id="data"`, "application/json", "\\u003c/script\\u003e", "textContent"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(s, "innerHTML") {
		t.Error("result data must never be rendered through innerHTML")
	}
	if strings.Contains(s, "http://") && strings.Contains(s, "src=\"http") || strings.Contains(s, "cdn.") {
		t.Error("the report must not load anything from the network")
	}
}

func TestEmbeddedJSONRoundTrips(t *testing.T) {
	r := sample()
	page, err := render(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := string(page)
	i := strings.Index(s, `<script id="data" type="application/json">`) + len(`<script id="data" type="application/json">`)
	j := strings.Index(s[i:], "</script>")
	var back results.Results
	if err := json.Unmarshal([]byte(s[i:i+j]), &back); err != nil {
		t.Fatalf("embedded data must be valid JSON: %v", err)
	}
	if back.Scenarios[0].Description != evil || back.Totals != r.Totals {
		t.Fatal("embedded data must equal the input exactly")
	}
}

func TestRejectsUnsupportedVersionMalformedAndInconsistentInput(t *testing.T) {
	dir := t.TempDir()
	r := sample()
	r.Version = 99
	if _, err := Build(write(t, dir, r), filepath.Join(dir, "o.html")); err == nil || !strings.Contains(err.Error(), "unsupported results version") {
		t.Fatalf("%v", err)
	}
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte("{not json"), 0o644)
	if _, err := Build(bad, filepath.Join(dir, "o.html")); err == nil {
		t.Fatal("malformed JSON must be rejected")
	}
	r2 := sample()
	r2.Totals.Assertions.Passed = 5 // does not match the recorded scenarios
	d2 := filepath.Join(dir, "inc")
	if _, err := Build(write(t, d2, r2), filepath.Join(d2, "o.html")); err == nil || !strings.Contains(err.Error(), "inconsistent") {
		t.Fatalf("inconsistent totals must be rejected: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d2, "o.html")); err == nil {
		t.Fatal("no report may be written for rejected input")
	}
}

func TestAttachmentsAreCopiedWhenTheReportMovesAndMissingOnesAreReported(t *testing.T) {
	src := t.TempDir()
	in := write(t, src, sample())
	dest := filepath.Join(t.TempDir(), "elsewhere", "report.html")

	res, err := Build(in, dest) // attachment file does not exist yet
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Missing) != 1 || res.Missing[0] != "attachments/0001-res.txt" || res.Copied != 0 {
		t.Fatalf("a missing attachment must be reported explicitly: %+v", res)
	}
	page, _ := os.ReadFile(dest)
	if !strings.Contains(string(page), `["attachments/0001-res.txt"]`) {
		t.Error("the report itself must say which attachment is missing")
	}

	os.MkdirAll(filepath.Join(src, "attachments"), 0o755)
	os.WriteFile(filepath.Join(src, "attachments", "0001-res.txt"), []byte("full body"), 0o644)
	res, err = Build(in, dest)
	if err != nil || res.Copied != 1 || len(res.Missing) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	got, err := os.ReadFile(filepath.Join(filepath.Dir(dest), "attachments", "0001-res.txt"))
	if err != nil || string(got) != "full body" {
		t.Fatalf("attachment must be copied next to the moved report: %v %q", err, got)
	}
}

func TestUnsafeAttachmentPathsAreRejected(t *testing.T) {
	for _, p := range []string{"../secret.txt", "/etc/passwd", "attachments/../../x", `attachments\x.txt`, "C:/x.txt", "other/x.txt", "attachments/./x"} {
		r := sample()
		r.Calls[0].ResBody.Attachment = p
		dir := t.TempDir()
		if _, err := Build(write(t, dir, r), filepath.Join(dir, "o.html")); err == nil || !strings.Contains(err.Error(), "unsafe attachment path") {
			t.Errorf("%q must be rejected, got %v", p, err)
		}
	}
}
