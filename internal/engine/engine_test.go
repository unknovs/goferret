package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/unknovs/goferret/internal/config"
	"github.com/unknovs/goferret/internal/results"
	"github.com/unknovs/goferret/internal/suite"
)

const inputsYAML = `version: 1
inputs:
  BASE: {type: url, required: true}
  KEY:  {type: string, required: true, secret: true}
  OPTKEY: {type: string, secret: true}
`

const header = `version: 1
name: T
inputs: inputs.yaml
fixtureRoot: fixtures
tags: [a, b, slow]
services:
  api:
    baseUrl: "{{BASE}}"
    headers: { APIKey: "{{KEY}}" }
    optionalHeaders: { X-Opt: "{{OPTKEY}}" }
scenarios: [s.yaml]
`

func load(t *testing.T, suiteYAML, scenarios string) *suite.Suite {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "fixtures"), 0o755)
	os.WriteFile(filepath.Join(dir, "fixtures", "f.txt"), []byte("hello"), 0o644)
	for name, body := range map[string]string{"inputs.yaml": inputsYAML, "suite.yaml": suiteYAML, "s.yaml": scenarios} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, err := suite.Load(filepath.Join(dir, "suite.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if errs := s.Validate(); len(errs) > 0 {
		t.Fatalf("suite should be valid: %v", errs)
	}
	return s
}

func inputs(base string) map[string]config.Value {
	return map[string]config.Value{
		"BASE": {Value: base},
		"KEY":  {Value: "s3cr3t-key-123", Secret: true},
	}
}

func run(t *testing.T, s *suite.Suite, in map[string]config.Value, sel Selection) (*results.Results, string) {
	t.Helper()
	out := t.TempDir()
	chosen, err := Select(s, sel)
	if err != nil {
		t.Fatal(err)
	}
	r := New(s, in, Options{OutDir: out})
	res := r.Run(context.Background(), chosen, sel)
	if err := results.WriteAll(out, res); err != nil {
		t.Fatal(err)
	}
	return res, out
}

func scenario(res *results.Results, id string) *results.ScenarioResult {
	for i := range res.Scenarios {
		if res.Scenarios[i].ID == id {
			return &res.Scenarios[i]
		}
	}
	return nil
}

func TestFlowTypedSubstitutionCapturesAndRedaction(t *testing.T) {
	var gotAPIKey, gotBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey.Store(r.Header.Get("APIKey"))
		b, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/create":
			w.Header().Set("X-Token", "tok-abcdef-123456")
			json.NewEncoder(w).Encode(map[string]any{"ticket": "D1", "count": 3, "echo": r.Header.Get("APIKey")})
		case "/confirm":
			gotBody.Store(string(b))
			w.Write([]byte(`{"ok":true,"items":[{"n":"a"},{"n":"b"}]}`))
		}
	}))
	defer srv.Close()

	s := load(t, header, `version: 1
scenarios:
  - id: flow
    tags: [a]
    vars:
      session: "sess-{{$random_hex 8}}"
      fh: "{{$file_sha256_b64 f.txt}}"
    steps:
      - id: create
        service: api
        method: POST
        path: /create
        json: { session: "{{session}}", file: "{{$file_base64 f.txt}}" }
        capture:
          ticket: { json: /ticket }
          count: { json: /count }
          token: { header: X-Token, secret: true }
        assert:
          - name: shape
            checks:
              - { json: /ticket, type: string, eq: D1 }
              - { json: /count, type: number, min: 1, max: 5 }
              - { header: X-Token, matches: '^tok-' }
      - id: confirm
        service: api
        method: POST
        path: /confirm
        json: { ticket: "{{ticket}}", count: "{{count}}", text: "n={{count}}", auth: "{{token}}", hash: "{{fh}}" }
        assert:
          - name: any element
            checks:
              - { json: /items, any: { json: /n, eq: b } }
              - { json: /items, length: 2 }
          - name: ok
            checks:
              - { json: /ok, eq: true }
`)
	res, out := run(t, s, inputs(srv.URL), Selection{})
	if !res.AllPassed() {
		b, _ := json.MarshalIndent(res.Scenarios, "", " ")
		t.Fatalf("expected all to pass:\n%s", b)
	}
	if gotAPIKey.Load() != "s3cr3t-key-123" {
		t.Fatalf("service default header must be sent, got %v", gotAPIKey.Load())
	}
	var body map[string]any
	json.Unmarshal([]byte(gotBody.Load().(string)), &body)
	if body["count"] != float64(3) {
		t.Errorf("an exact {{var}} must keep its JSON type (number), got %T %v", body["count"], body["count"])
	}
	if body["text"] != "n=3" {
		t.Errorf("interpolation inside a string: %v", body["text"])
	}
	if body["hash"] != "LPJNul+wow4m6DsqxbninhsWHlwfp0JecwQzYpOLmCQ=" {
		t.Errorf("sha256 of fixture, base64: %v", body["hash"])
	}
	if res.Totals.Assertions.Total != 3 || res.Totals.Assertions.Passed != 3 {
		t.Errorf("totals: %+v", res.Totals)
	}

	// Redaction: no secret value in any persisted artifact.
	for _, f := range []string{"results.json", "junit.xml"} {
		data, _ := os.ReadFile(filepath.Join(out, f))
		for _, secret := range []string{"s3cr3t-key-123", "tok-abcdef-123456"} {
			if strings.Contains(string(data), secret) {
				t.Errorf("%s leaks %s", f, secret)
			}
		}
	}
	data, _ := os.ReadFile(filepath.Join(out, "results.json"))
	if !strings.Contains(string(data), "[REDACTED]") {
		t.Error("expected redaction markers (header APIKey, echoed key, captured token)")
	}
}

func TestFailureKeepsAllAssertionsBlocksDependentsRunsCleanup(t *testing.T) {
	var cleaned atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cleanup" {
			cleaned.Add(1)
		}
		w.Write([]byte(`{"v":1}`))
	}))
	defer srv.Close()
	s := load(t, header, `version: 1
scenarios:
  - id: broken
    tags: [a]
    steps:
      - id: one
        service: api
        method: GET
        path: /x
        assert:
          - name: first fails
            checks: [ { json: /v, eq: 2 } ]
          - name: second still evaluated
            checks: [ { json: /v, eq: 1 } ]
      - id: two
        service: api
        method: GET
        path: /never
        assert:
          - name: depends on step one
            checks: [ { status: 200 } ]
    cleanup:
      - id: clean
        service: api
        method: DELETE
        path: /cleanup
  - id: independent
    tags: [b]
    steps:
      - id: ok
        service: api
        method: GET
        path: /x
        assert:
          - name: fine
            checks: [ { status: 200 } ]
`)
	res, _ := run(t, s, inputs(srv.URL), Selection{})
	b := scenario(res, "broken")
	if b.Status != results.Failed {
		t.Fatalf("broken: %s %s", b.Status, b.Reason)
	}
	g := b.Steps[0].Groups
	if len(g) != 2 || g[0].Status != results.Failed || g[1].Status != results.Passed {
		t.Fatalf("all assertions of an executed step are evaluated: %+v", g)
	}
	if b.Steps[1].Status != results.Blocked || b.Steps[1].Groups[0].Status != results.Blocked {
		t.Fatalf("dependent step must be blocked: %+v", b.Steps[1])
	}
	if cleaned.Load() != 1 {
		t.Fatalf("cleanup must run exactly once, ran %d", cleaned.Load())
	}
	if scenario(res, "independent").Status != results.Passed {
		t.Fatal("independent scenario must still run")
	}
	// total == passed+failed+errored+blocked
	a := res.Totals.Assertions
	if a.Total != a.Passed+a.Failed+a.Errored+a.Blocked || a.Total != 4 || a.Failed != 1 || a.Blocked != 1 || a.Passed != 2 {
		t.Fatalf("totals: %+v", a)
	}
	if res.AllPassed() {
		t.Fatal("run must not be successful")
	}
}

func TestScenarioDependencyAndSetupBlocking(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			w.WriteHeader(500)
			return
		}
		w.Write([]byte(`{"id":"X1"}`))
	}))
	defer srv.Close()
	sy := strings.Replace(header, "scenarios: [s.yaml]", `scenarios: [s.yaml]
setup:
  - id: boot
    service: api
    method: GET
    path: /boot
    capture: { shared_id: { json: /id } }
    assert: [ { name: boot ok, checks: [ { status: 200 } ] } ]`, 1)
	sc := `version: 1
scenarios:
  - id: parent
    exports: [made]
    steps:
      - id: p
        service: api
        method: GET
        path: /bad
        capture: { made: { json: /id } }
        assert: [ { name: parent ok, checks: [ { status: 200 } ] } ]
  - id: child
    dependsOn: [parent]
    steps:
      - id: c
        service: api
        method: GET
        path: "/child/{{made}}"
        assert: [ { name: child ok, checks: [ { status: 200 } ] } ]
  - id: uses-setup
    steps:
      - id: u
        service: api
        method: GET
        path: "/setup/{{shared_id}}"
        assert: [ { name: sees setup capture, checks: [ { status: 200 } ] } ]
`
	res, _ := run(t, load(t, sy, sc), inputs(srv.URL), Selection{})
	if st := scenario(res, "parent").Status; st != results.Errored && st != results.Failed {
		t.Fatalf("parent: %s", st)
	}
	if c := scenario(res, "child"); c.Status != results.Blocked || !strings.Contains(c.Reason, "parent") {
		t.Fatalf("child must be blocked by parent: %+v", c)
	}
	if scenario(res, "uses-setup").Status != results.Passed {
		t.Fatal("setup captures are visible to independent scenarios")
	}
	if res.Totals.Assertions.Blocked != 1 {
		t.Fatalf("blocked assertion groups are counted: %+v", res.Totals.Assertions)
	}

	// A failing setup blocks everything.
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer failing.Close()
	res, _ = run(t, load(t, sy, sc), inputs(failing.URL), Selection{})
	if scenario(res, "setup").Status == results.Passed {
		t.Fatal("setup should not pass")
	}
	for _, id := range []string{"parent", "child", "uses-setup"} {
		if scenario(res, id).Status != results.Blocked {
			t.Errorf("%s must be blocked when setup fails", id)
		}
	}
}

func TestSelectionByTagScenarioAndDependencyClosure(t *testing.T) {
	s := load(t, header, `version: 1
scenarios:
  - { id: base, tags: [b], steps: [ { id: s, service: api, method: GET, path: /x } ] }
  - { id: top, tags: [a], dependsOn: [base], steps: [ { id: s, service: api, method: GET, path: /x } ] }
  - { id: other, tags: [a, slow], steps: [ { id: s, service: api, method: GET, path: /x } ] }
`)
	sel, err := Select(s, Selection{Tags: []string{"a"}, ExcludeTags: []string{"slow"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sel) != 2 || sel[0].S.ID != "base" || !sel[0].ByDependency || sel[1].S.ID != "top" || sel[1].ByDependency {
		t.Fatalf("closure must add base, marked by dependency: %+v", sel)
	}
	if _, err := Select(s, Selection{Tags: []string{"nope"}}); err == nil {
		t.Error("unknown tag must be an error")
	}
	if _, err := Select(s, Selection{Scenarios: []string{"tp"}}); err == nil {
		t.Error("unknown scenario must be an error")
	}
	all, _ := Select(s, Selection{})
	if len(all) != 3 {
		t.Fatalf("no filter selects everything: %d", len(all))
	}
}

func TestNeededInputsOnlyForSelection(t *testing.T) {
	s := load(t, header, `version: 1
scenarios:
  - { id: plain, tags: [a], steps: [ { id: s, service: api, method: GET, path: /x } ] }
  - { id: withopt, tags: [b], steps: [ { id: s, service: api, method: GET, path: "/x?k={{OPTKEY}}" } ] }
`)
	sel, _ := Select(s, Selection{Tags: []string{"a"}})
	req, opt := NeededInputs(s, sel)
	has := func(l []string, v string) bool {
		for _, x := range l {
			if x == v {
				return true
			}
		}
		return false
	}
	if !has(req, "BASE") || !has(req, "KEY") || has(req, "OPTKEY") {
		t.Fatalf("required: %v", req)
	}
	if !has(opt, "OPTKEY") {
		t.Fatalf("optional header input should be resolved when present: %v", opt)
	}
	sel, _ = Select(s, Selection{Tags: []string{"b"}})
	req, _ = NeededInputs(s, sel)
	if !has(req, "OPTKEY") {
		t.Fatalf("a scenario that references OPTKEY requires it: %v", req)
	}
}

func TestTransportErrorIsRecordedAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens any more
	s := load(t, header, `version: 1
scenarios:
  - id: down
    steps:
      - id: a
        service: api
        method: GET
        path: /x
        assert: [ { name: reachable, checks: [ { status: 200 } ] } ]
      - id: b
        service: api
        method: GET
        path: /y
        assert: [ { name: after, checks: [ { status: 200 } ] } ]
`)
	res, _ := run(t, s, inputs(url), Selection{})
	d := scenario(res, "down")
	if d.Steps[0].Status != results.Errored || d.Steps[1].Status != results.Blocked {
		t.Fatalf("%+v", d.Steps)
	}
	if len(res.Calls) != 1 || res.Calls[0].Error == "" {
		t.Fatalf("a failed transport attempt must be recorded: %+v", res.Calls)
	}
}

func TestRedirectsAreNotFollowedAndMutatingCallsNotRetried(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	s := load(t, header, `version: 1
scenarios:
  - id: redir
    steps:
      - id: post
        service: api
        method: POST
        path: /x
        json: { a: 1 }
        assert: [ { name: redirect is the result, checks: [ { status: 307 } ] } ]
`)
	res, _ := run(t, s, inputs(srv.URL), Selection{})
	if hits.Load() != 1 || !res.AllPassed() {
		t.Fatalf("hits=%d passed=%v", hits.Load(), res.AllPassed())
	}
}

func TestPollIsBoundedAndRecorded(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) >= 2 {
			w.Write([]byte("true"))
			return
		}
		w.Write([]byte("false"))
	}))
	defer srv.Close()
	s := load(t, header, `version: 1
scenarios:
  - id: ready
    steps:
      - id: wait
        service: api
        method: GET
        path: /ready
        poll: { attempts: 3, intervalSeconds: 1, until: [ { json: "", eq: true } ] }
        assert: [ { name: ready, checks: [ { json: "", eq: true } ] } ]
`)
	res, _ := run(t, s, inputs(srv.URL), Selection{})
	if !res.AllPassed() || len(res.Calls) != 2 || res.Calls[1].Attempt != 2 {
		t.Fatalf("calls=%d %+v", len(res.Calls), res.Scenarios)
	}

	n.Store(-100) // never ready
	res, _ = run(t, s, inputs(srv.URL), Selection{})
	st := scenario(res, "ready").Steps[0]
	if st.Status != results.Errored || !strings.Contains(st.Reason, "3 attempts") || len(res.Calls) != 3 {
		t.Fatalf("%+v calls=%d", st, len(res.Calls))
	}
}

func TestLargeBodyGoesToRedactedAttachment(t *testing.T) {
	big := strings.Repeat("A", 20000) // a long base64 value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"blob":"` + big + `","note":"the key is s3cr3t-key-123 ok"}`))
	}))
	defer srv.Close()
	s := load(t, header, `version: 1
scenarios:
  - id: big
    steps:
      - id: g
        service: api
        method: GET
        path: /x
        assert: [ { name: ok, checks: [ { status: 200 } ] } ]
`)
	res, out := run(t, s, inputs(srv.URL), Selection{})
	b := res.Calls[0].ResBody
	// the 20 KB run of "A" is valid base64 text and becomes a short marker; the secret next to it is redacted
	if b.Truncated || b.Attachment != "" || len(b.Preview) > 400 {
		t.Fatalf("a long base64 value must collapse to a short marker, with nothing written aside: %+v", b)
	}
	if strings.Contains(b.Preview, "s3cr3t-key-123") || !strings.Contains(b.Preview, "[REDACTED]") {
		t.Fatalf("the secret must be redacted: %q", b.Preview)
	}
	if _, err := os.Stat(filepath.Join(out, "attachments")); err == nil {
		t.Fatal("a run must not create an attachments folder")
	}
}

func TestVeryLongNonBase64BodyIsCutAndFlagged(t *testing.T) {
	long := strings.Repeat("line of ordinary text, not base64! ", 4000) // about 140 KB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"text":"` + long + `"}`))
	}))
	defer srv.Close()
	s := load(t, header, `version: 1
scenarios:
  - id: long
    steps: [ { id: g, service: api, method: GET, path: /x, assert: [ { name: ok, checks: [ { status: 200 } ] } ] } ]
`)
	res, out := run(t, s, inputs(srv.URL), Selection{})
	b := res.Calls[0].ResBody
	if !b.Truncated || len(b.Preview) > PreviewBytes+10 || b.Bytes < 100000 {
		t.Fatalf("bodies are cut at PreviewBytes and say so: %+v", b)
	}
	if _, err := os.Stat(filepath.Join(out, "attachments")); err == nil {
		t.Fatal("no attachments folder, even for a cut body")
	}
}

func TestMissingCaptureIsAnErrorNotEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"a":1}`)) }))
	defer srv.Close()
	s := load(t, header, `version: 1
scenarios:
  - id: cap
    steps:
      - id: one
        service: api
        method: GET
        path: /x
        capture: { v: { json: /missing } }
        assert: [ { name: ok, checks: [ { json: /a, eq: 1 } ] } ]
      - id: two
        service: api
        method: GET
        path: "/y/{{v}}"
`)
	res, _ := run(t, s, inputs(srv.URL), Selection{})
	c := scenario(res, "cap")
	if c.Steps[0].Status != results.Errored || !strings.Contains(c.Steps[0].Reason, "capture v") || c.Steps[1].Status != results.Blocked {
		t.Fatalf("%+v", c.Steps)
	}
	if c.Steps[0].Groups[0].Status != results.Passed {
		t.Fatal("assertions still evaluate when a capture fails")
	}
}

func TestOptionalHeaderOmittedWhenInputUnset(t *testing.T) {
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Header.Values("X-Opt"))
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	s := load(t, header, `version: 1
scenarios:
  - { id: o, steps: [ { id: s, service: api, method: GET, path: /x, assert: [ { name: ok, checks: [ { status: 200 } ] } ] } ] }
`)
	run(t, s, inputs(srv.URL), Selection{})
	if len(got.Load().([]string)) != 0 {
		t.Fatalf("header must be omitted when its input is unset: %v", got.Load())
	}
	in := inputs(srv.URL)
	in["OPTKEY"] = config.Value{Value: "optional-key-777", Secret: true}
	run(t, s, in, Selection{})
	if v := got.Load().([]string); len(v) != 1 || v[0] != "optional-key-777" {
		t.Fatalf("header must be sent when set: %v", v)
	}
}

func TestFullRunOnlyScenariosAreSkippedInFilteredRuns(t *testing.T) {
	sy := strings.Replace(header, "tags: [a, b, slow]", "tags: [a, b, slow]\nfullRunOnlyTags: [slow]", 1)
	s := load(t, sy, `version: 1
scenarios:
  - { id: normal, tags: [a], steps: [ { id: s, service: api, method: GET, path: /x } ] }
  - { id: marker, tags: [slow], steps: [ { id: s, service: api, method: GET, path: /x } ] }
`)
	ids := func(sel Selection) string {
		chosen, err := Select(s, sel)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range chosen {
			out = append(out, c.S.ID)
		}
		return strings.Join(out, ",")
	}
	if got := ids(Selection{}); got != "normal,marker" {
		t.Errorf("a full run includes everything: %s", got)
	}
	if got := ids(Selection{Tags: []string{"a"}}); got != "normal" {
		t.Errorf("a tag-filtered run skips full-run-only scenarios: %s", got)
	}
	if got := ids(Selection{ExcludeTags: []string{"b"}}); got != "normal" {
		t.Errorf("any filter makes the run partial: %s", got)
	}
	if got := ids(Selection{Tags: []string{"slow"}}); got != "marker" {
		t.Errorf("selecting the tag explicitly runs them: %s", got)
	}
	if got := ids(Selection{Scenarios: []string{"marker"}}); got != "marker" {
		t.Errorf("selecting the scenario explicitly runs it: %s", got)
	}
}
