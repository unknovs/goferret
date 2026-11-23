package engine

import (
	"net/http"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/unknovs/goferret/internal/results"
	"github.com/unknovs/goferret/internal/suite"
)

// check builds a suite.Check from YAML so the tests use the real file syntax.
func check(t *testing.T, y string) suite.Check {
	t.Helper()
	var c suite.Check
	if err := yaml.Unmarshal([]byte(y), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestOperators(t *testing.T) {
	body := `{"s":"hello","n":5,"b":true,"nul":null,"arr":[1,2,3],"empty":[],"obj":{"k":"v"},"items":[{"e":"x"},{"e":"y"}],"a":7,"c":7,"d":8}`
	resp := &Response{Status: 200, Header: http.Header{"X-H": {"val"}}, Body: []byte(body)}
	ev := &Evaluator{R: &Renderer{}}
	sc := NewScope(nil)
	sc.Set("want", Var{Value: "hello"})
	sc.Set("five", Var{Value: float64(5)})

	pass := []string{
		"{status: 200}",
		"{json: /s, eq: hello}", "{json: /s, eq: '{{want}}'}", "{json: /n, eq: 5}", "{json: /n, eq: '{{five}}'}",
		"{json: /b, eq: true}", "{json: /nul, eq: null}", "{json: /nul, type: 'null'}",
		"{json: /s, type: string}", "{json: /n, type: number}", "{json: /arr, type: array}", "{json: /obj, type: object}", "{json: /b, type: boolean}",
		"{json: /s, exists: true}", "{json: /nothing, exists: false}", "{json: /nothing, absent: true}",
		"{json: /s, matches: '^h.llo$'}", "{json: /n, min: 5, max: 5}", "{json: /arr, length: 3}", "{json: /arr, lengthMin: 2}",
		"{json: /empty, empty: true}", "{json: /arr, empty: false}", "{json: /s, contains: ell}", "{json: /arr, contains: 2}",
		"{json: /a, eqJson: /c}", "{json: /a, ne: 8}", "{json: /a, neJson: /d}",
		"{json: /items, any: {json: /e, eq: y}}", "{json: /arr, any: {json: '', min: 3}}",
		"{json: /arr, all: {json: '', type: number}}", "{json: /n, in: [1, 5, 9]}", "{json: /s, in: [nope, hello]}", "{json: /arr, all: {json: '', in: [1, 2, 3, 4]}}", "{json: /items, all: {json: /e, type: string}}", "{json: /empty, all: {json: '', eq: 1}}",
		"{header: X-H, eq: val}", "{header: X-H, exists: true}", "{header: Nope, absent: true}",
		"{json: /s, type: string, eq: hello, matches: ^h}",
	}
	for _, y := range pass {
		for _, r := range ev.Run(check(t, y), resp, sc) {
			if r.Status != results.Passed {
				t.Errorf("%s should pass: %+v", y, r)
			}
		}
	}

	fail := []string{
		"{status: 404}", "{json: /s, eq: nope}", "{json: /n, eq: '5'}", "{json: /s, type: number}", "{json: /s, exists: false}",
		"{json: /nothing, exists: true}", "{json: /nothing, eq: 1}", "{json: /s, matches: '^x'}", "{json: /n, matches: x}",
		"{json: /n, min: 6}", "{json: /n, max: 4}", "{json: /s, min: 1}", "{json: /arr, length: 2}", "{json: /arr, empty: true}",
		"{json: /s, contains: zzz}", "{json: /arr, contains: 9}", "{json: /a, eqJson: /d}", "{json: /a, ne: 7}", "{json: /a, neJson: /c}",
		"{json: /items, any: {json: /e, eq: z}}", "{header: X-H, eq: other}", "{header: Nope, exists: true}",
		"{json: /nothing, any: {json: '', eq: 1}}",
		"{json: /arr, all: {json: '', min: 2}}", "{json: /n, in: [1, 2]}", "{json: /s, in: [a, b]}", "{json: /nothing, in: [1]}", "{json: /arr, all: {json: '', in: [1, 2]}}", "{json: /items, all: {json: /e, eq: x}}", "{json: /nothing, all: {json: '', eq: 1}}", "{json: /s, all: {json: '', eq: 1}}",
	}
	for _, y := range fail {
		rs := ev.Run(check(t, y), resp, sc)
		bad := false
		for _, r := range rs {
			if r.Status == results.Failed {
				bad = true
			}
			if r.Status == results.Errored {
				t.Errorf("%s: should fail, not error: %+v", y, r)
			}
		}
		if !bad {
			t.Errorf("%s should fail: %+v", y, rs)
		}
	}
}

func TestEveryOperatorIsEvaluatedAndReportedSeparately(t *testing.T) {
	resp := &Response{Status: 200, Body: []byte(`{"s":"hello"}`)}
	ev := &Evaluator{R: &Renderer{}}
	rs := ev.Run(check(t, "{json: /s, type: number, eq: hello, matches: '^x'}"), resp, NewScope(nil))
	if len(rs) != 3 || rs[0].Status != results.Failed || rs[1].Status != results.Passed || rs[2].Status != results.Failed {
		t.Fatalf("one result per operator, none skipped after a failure: %+v", rs)
	}
}

func TestNonJSONBodyAndBadReferencesAreErrorsNotFailures(t *testing.T) {
	ev := &Evaluator{R: &Renderer{}}
	sc := NewScope(nil)
	rs := ev.Run(check(t, "{json: /a, eq: 1}"), &Response{Status: 200, Body: []byte("<html>")}, sc)
	if len(rs) != 1 || rs[0].Status != results.Errored {
		t.Fatalf("a non-JSON body is an execution error: %+v", rs)
	}
	rs = ev.Run(check(t, "{json: /a, eq: '{{missing}}'}"), &Response{Status: 200, Body: []byte(`{"a":1}`)}, sc)
	if len(rs) != 1 || rs[0].Status != results.Errored {
		t.Fatalf("an undefined variable is an error, never an empty string: %+v", rs)
	}
}

func TestVarAndBodyTargets(t *testing.T) {
	ev := &Evaluator{R: &Renderer{}}
	sc := NewScope(nil)
	sc.Set("signed", Var{Value: "eyJwYXlsb2FkIjoiRU5WRUxPUElORyIsIm4iOjJ9"}) // {"payload":"ENVELOPING","n":2}
	sc.Set("obj", Var{Value: map[string]any{"a": "x"}})
	resp := &Response{Status: 200, Body: []byte("true")}
	for _, y := range []string{
		"{var: signed, decodeJson: /payload, eq: ENVELOPING}",
		"{var: signed, decodeJson: /n, type: number}",
		"{var: obj, json: /a, eq: x}",
		"{body: true, contains: 'tr'}",
		"{body: true, eq: 'true'}",
	} {
		for _, r := range ev.Run(check(t, y), resp, sc) {
			if r.Status != results.Passed {
				t.Errorf("%s should pass: %+v", y, r)
			}
		}
	}
	rs := ev.Run(check(t, "{var: nope, exists: true}"), resp, sc)
	if len(rs) != 1 || rs[0].Status != results.Errored {
		t.Errorf("an undefined variable target is an error: %+v", rs)
	}
	rs = ev.Run(check(t, "{body: true, contains: 'zzz'}"), resp, sc)
	if rs[0].Status != results.Failed {
		t.Errorf("%+v", rs)
	}
}
