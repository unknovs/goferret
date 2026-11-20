package docs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unknovs/goferret/internal/suite"
)

func load(t *testing.T) *suite.Suite {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"inputs.yaml": "version: 1\ninputs:\n  BASE: {type: url, required: true, description: 'base | url'}\n  KEY: {secret: true}\n",
		"suite.yaml":  "version: 1\nname: D\ninputs: inputs.yaml\ntags: [a, b]\nservices:\n  api: {baseUrl: '{{BASE}}'}\nscenarios: [s.yaml]\n",
		"s.yaml": `version: 1
scenarios:
  - id: one
    description: First <scenario>
    tags: [a]
    steps:
      - id: main
        service: api
        method: GET
        path: /x
        assert: [ { name: Status is 200, checks: [ { status: 200 } ] }, { name: Has body, checks: [ { json: /a, exists: true } ] } ]
      - id: main-sign
        helper: true
        service: api
        method: POST
        path: /sign
        assert: [ { name: helper check, checks: [ { status: 200 } ] } ]
  - id: two
    tags: [b, a]
    steps:
      - { id: s, service: api, method: GET, path: /y, assert: [ { name: only one, checks: [ { status: 200 } ] } ] }
`,
	}
	for n, b := range files {
		os.WriteFile(filepath.Join(dir, n), []byte(b), 0o644)
	}
	s, err := suite.Load(filepath.Join(dir, "suite.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestGenerate(t *testing.T) {
	out := Generate(load(t))
	for _, want := range []string{
		"| `BASE` | url | yes | no | base \\| url |",
		"| `KEY` | string | no | yes |",
		"| `a` | 2 | 3 |", // helper checks are not counted
		"| `b` | 1 | 1 |",
		"All scenarios: 2, assertions: 3",
		"<code>one</code> &mdash; First &lt;scenario&gt; (2 assertions)",
		"- **main-sign** `POST /sign` _(helper)_",
		"  - Status is 200",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Count(out, "<code>two</code>") != 1 || strings.Index(out, "<code>two</code>") < strings.Index(out, "#### b") {
		t.Error("a scenario is listed once, under its first tag (two -> b, not a)")
	}
	if Generate(load(t)) != out {
		t.Error("generation must be deterministic")
	}
}

func TestReplace(t *testing.T) {
	readme := "intro\n" + Begin + "\nold\n" + End + "\noutro\n"
	got, err := Replace(readme, "NEW\n")
	if err != nil || !strings.Contains(got, "NEW") || strings.Contains(got, "old") || !strings.HasPrefix(got, "intro\n") || !strings.HasSuffix(got, "outro\n") {
		t.Fatalf("%q %v", got, err)
	}
	again, _ := Replace(got, "NEW\n")
	if again != got {
		t.Error("replace must be idempotent")
	}
	if _, err := Replace("no markers", "x"); err == nil {
		t.Error("missing markers must be an error")
	}
	if _, err := Replace(End+"\n"+Begin, "x"); err == nil {
		t.Error("reversed markers must be an error")
	}
}
