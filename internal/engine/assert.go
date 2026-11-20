package engine

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/unknovs/goferret/internal/jsonptr"
	"github.com/unknovs/goferret/internal/results"
	"github.com/unknovs/goferret/internal/suite"
)

// Response is a received HTTP response with lazily parsed JSON.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
	// Duration is the time to receive the full response.
	Duration time.Duration

	parsed bool
	doc    any
	docErr error
}

// JSON parses the body once.
func (r *Response) JSON() (any, error) {
	if !r.parsed {
		r.parsed = true
		if len(r.Body) == 0 {
			r.docErr = fmt.Errorf("response body is empty")
		} else if err := json.Unmarshal(r.Body, &r.doc); err != nil {
			r.docErr = fmt.Errorf("response body is not valid JSON: %v", err)
		}
	}
	return r.doc, r.docErr
}

// Evaluator runs checks, rendering templated expectations.
type Evaluator struct {
	R *Renderer
}

type target struct {
	doc   any // the JSON document the pointer is relative to (nil for header targets)
	desc  string
	val   any
	found bool
}

// Run evaluates every operator of one check and returns one result per
// operator. All operators are evaluated even if an earlier one fails.
func (ev *Evaluator) Run(c suite.Check, resp *Response, sc *Scope) []results.CheckResult {
	if c.Status != nil {
		ok := resp.Status == *c.Status
		r := results.CheckResult{Desc: fmt.Sprintf("status == %d", *c.Status), Status: results.Passed}
		if !ok {
			r.Status, r.Message = results.Failed, fmt.Sprintf("expected status %d, got %d", *c.Status, resp.Status)
		}
		return []results.CheckResult{r}
	}
	if c.ResponseTimeLtMs != nil {
		ms := resp.Duration.Milliseconds()
		r := results.CheckResult{Desc: fmt.Sprintf("response time < %d ms", *c.ResponseTimeLtMs), Status: results.Passed}
		if ms >= int64(*c.ResponseTimeLtMs) {
			r.Status, r.Message = results.Failed, fmt.Sprintf("response took %d ms", ms)
		}
		return []results.CheckResult{r}
	}
	var t target
	switch {
	case c.Body:
		t = target{desc: "body", val: string(resp.Body), found: true}
	case c.Header != "":
		vals := resp.Header.Values(c.Header)
		t = target{desc: "header " + c.Header, found: len(vals) > 0}
		if len(vals) > 0 {
			t.val = vals[0]
		}
	default:
		var doc any
		if c.Var != "" {
			v, ok := sc.Get(c.Var)
			if !ok {
				return []results.CheckResult{{Desc: "var " + c.Var, Status: results.Errored, Message: "variable is not defined"}}
			}
			doc = v.Value
		} else {
			var err error
			if doc, err = resp.JSON(); err != nil {
				return []results.CheckResult{{Desc: "json " + c.JSON, Status: results.Errored, Message: err.Error()}}
			}
		}
		v, found, perr := jsonptr.Get(doc, c.JSON)
		if perr != nil {
			return []results.CheckResult{{Desc: "json " + c.JSON, Status: results.Errored, Message: perr.Error()}}
		}
		label := "json " + orRoot(c.JSON)
		if c.Var != "" {
			label = "var " + c.Var + " " + orRoot(c.JSON)
		}
		t = target{doc: doc, desc: label, val: v, found: found}
		if c.DecodeJSON != nil {
			inner, derr := decodeBase64JSON(v, found)
			if derr != nil {
				return []results.CheckResult{{Desc: "json " + orRoot(c.JSON) + " (decoded)", Status: results.Errored, Message: derr.Error()}}
			}
			iv, ifound, perr := jsonptr.Get(inner, *c.DecodeJSON)
			if perr != nil {
				return []results.CheckResult{{Desc: "json " + orRoot(c.JSON) + " (decoded)", Status: results.Errored, Message: perr.Error()}}
			}
			t = target{doc: inner, desc: "json " + orRoot(c.JSON) + " decoded " + orRoot(*c.DecodeJSON), val: iv, found: ifound}
		}
	}
	return ev.ops(c, t, sc)
}

// decodeBase64JSON decodes base64 text holding a JSON document.
func decodeBase64JSON(v any, found bool) (any, error) {
	if !found {
		return nil, fmt.Errorf("location does not exist in the response")
	}
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("expected base64 text, got %s", typeName(v))
	}
	var raw []byte
	var err error
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if raw, err = enc.DecodeString(s); err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("value is not valid base64")
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decoded value is not valid JSON: %v", err)
	}
	return doc, nil
}

func orRoot(p string) string {
	if p == "" {
		return "(root)"
	}
	return p
}

func (ev *Evaluator) ops(c suite.Check, t target, sc *Scope) []results.CheckResult {
	var out []results.CheckResult
	add := func(op string, ok bool, msg string) {
		r := results.CheckResult{Desc: t.desc + " " + op, Status: results.Passed}
		if !ok {
			r.Status, r.Message = results.Failed, msg
		}
		out = append(out, r)
	}
	errRes := func(op string, err error) {
		out = append(out, results.CheckResult{Desc: t.desc + " " + op, Status: results.Errored, Message: err.Error()})
	}
	missing := func(op string) { add(op, false, "location does not exist in the response") }

	if c.Exists != nil {
		add(fmt.Sprintf("exists=%v", *c.Exists), t.found == *c.Exists, fmt.Sprintf("exists is %v, expected %v", t.found, *c.Exists))
	}
	if c.Absent != nil {
		add(fmt.Sprintf("absent=%v", *c.Absent), (!t.found) == *c.Absent, fmt.Sprintf("absent is %v, expected %v", !t.found, *c.Absent))
	}
	if c.Type != "" {
		switch {
		case !t.found:
			missing("type " + c.Type)
		default:
			got := typeName(t.val)
			add("type "+c.Type, got == c.Type, fmt.Sprintf("expected type %s, got %s", c.Type, got))
		}
	}
	if c.Eq.Kind != 0 {
		want, err := ev.expected(&c.Eq, sc)
		switch {
		case err != nil:
			errRes("eq", err)
		case !t.found:
			missing("eq")
		default:
			add("eq", equalJSON(t.val, want), fmt.Sprintf("expected %s, got %s", show(want), show(t.val)))
		}
	}
	if c.EqJSON != "" {
		other, ofound, perr := jsonptr.Get(t.doc, c.EqJSON)
		switch {
		case perr != nil:
			errRes("eqJson", perr)
		case !t.found || !ofound:
			missing("eqJson " + c.EqJSON)
		default:
			add("equals "+c.EqJSON, equalJSON(t.val, other), fmt.Sprintf("%s differs from %s (%s)", show(t.val), c.EqJSON, show(other)))
		}
	}
	if c.NeJSON != "" {
		other, ofound, perr := jsonptr.Get(t.doc, c.NeJSON)
		switch {
		case perr != nil:
			errRes("neJson", perr)
		case !t.found || !ofound:
			missing("neJson " + c.NeJSON)
		default:
			add("differs from "+c.NeJSON, !equalJSON(t.val, other), fmt.Sprintf("%s must differ from %s", show(t.val), c.NeJSON))
		}
	}
	if c.Ne.Kind != 0 {
		want, err := ev.expected(&c.Ne, sc)
		switch {
		case err != nil:
			errRes("ne", err)
		case !t.found:
			missing("ne")
		default:
			add("ne", !equalJSON(t.val, want), fmt.Sprintf("value must not equal %s", show(want)))
		}
	}
	if c.Matches != "" {
		pat, err := ev.R.Str(c.Matches, sc)
		var re *regexp.Regexp
		if err == nil {
			re, err = regexp.Compile(pat)
		}
		switch {
		case err != nil:
			errRes("matches", err)
		case !t.found:
			missing("matches")
		default:
			s, isStr := t.val.(string)
			if !isStr {
				add("matches", false, fmt.Sprintf("expected a string, got %s", typeName(t.val)))
			} else {
				add("matches "+pat, re.MatchString(s), fmt.Sprintf("%s does not match %s", show(s), pat))
			}
		}
	}
	num := func(op string, bound float64, cmp func(a, b float64) bool, word string) {
		f, isNum := t.val.(float64)
		switch {
		case !t.found:
			missing(op)
		case !isNum:
			add(op, false, fmt.Sprintf("expected a number, got %s", typeName(t.val)))
		default:
			add(fmt.Sprintf("%s %v", op, bound), cmp(f, bound), fmt.Sprintf("%v is not %s %v", f, word, bound))
		}
	}
	if c.Min != nil {
		num("min", *c.Min, func(a, b float64) bool { return a >= b }, ">=")
	}
	if c.Max != nil {
		num("max", *c.Max, func(a, b float64) bool { return a <= b }, "<=")
	}
	if c.Length != nil || c.LengthMin != nil {
		n, ok := lengthOf(t.val)
		switch {
		case !t.found:
			missing("length")
		case !ok:
			add("length", false, fmt.Sprintf("%s has no length", typeName(t.val)))
		default:
			if c.Length != nil {
				add(fmt.Sprintf("length == %d", *c.Length), n == *c.Length, fmt.Sprintf("length is %d, expected %d", n, *c.Length))
			}
			if c.LengthMin != nil {
				add(fmt.Sprintf("length >= %d", *c.LengthMin), n >= *c.LengthMin, fmt.Sprintf("length is %d, expected at least %d", n, *c.LengthMin))
			}
		}
	}
	if c.Empty != nil {
		n, ok := lengthOf(t.val)
		switch {
		case !t.found:
			missing("empty")
		case !ok:
			add("empty", false, fmt.Sprintf("%s cannot be empty or non-empty", typeName(t.val)))
		default:
			add(fmt.Sprintf("empty=%v", *c.Empty), (n == 0) == *c.Empty, fmt.Sprintf("empty is %v, expected %v", n == 0, *c.Empty))
		}
	}
	if c.Contains.Kind != 0 {
		want, err := ev.expected(&c.Contains, sc)
		switch {
		case err != nil:
			errRes("contains", err)
		case !t.found:
			missing("contains")
		default:
			ok, cerr := containsValue(t.val, want)
			if cerr != nil {
				add("contains", false, cerr.Error())
			} else {
				add("contains "+show(want), ok, fmt.Sprintf("%s does not contain %s", show(t.val), show(want)))
			}
		}
	}
	if c.Any != nil {
		arr, isArr := t.val.([]any)
		switch {
		case !t.found:
			missing("any")
		case !isArr:
			add("any", false, fmt.Sprintf("expected an array, got %s", typeName(t.val)))
		default:
			var perr error
			matched, lastErr := false, ""
			for _, el := range arr {
				v, found, e := jsonptr.Get(el, c.Any.JSON)
				if e != nil {
					perr = e
					break
				}
				ok := true
				for _, r := range ev.ops(*c.Any, target{doc: el, desc: "element", val: v, found: found}, sc) {
					if r.Status != results.Passed {
						ok = false
						lastErr = r.Message
					}
				}
				if ok {
					matched = true
					break
				}
			}
			if perr != nil {
				errRes("any", perr)
			} else {
				add("any element matches", matched, fmt.Sprintf("no element of %d satisfied the check (last: %s)", len(arr), lastErr))
			}
		}
	}
	return out
}

func (ev *Evaluator) expected(n *yaml.Node, sc *Scope) (any, error) {
	var v any
	if err := n.Decode(&v); err != nil {
		return nil, err
	}
	r, err := ev.R.Any(v, sc)
	if err != nil {
		return nil, err
	}
	return normalize(r), nil
}

// normalize converts YAML-decoded numbers to float64 so they compare with
// encoding/json numbers.
func normalize(v any) any {
	switch t := v.(type) {
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case uint64:
		return float64(t)
	case float32:
		return float64(t)
	case []any:
		o := make([]any, len(t))
		for i, x := range t {
			o[i] = normalize(x)
		}
		return o
	case map[string]any:
		o := make(map[string]any, len(t))
		for k, x := range t {
			o[k] = normalize(x)
		}
		return o
	}
	return v
}

func equalJSON(a, b any) bool { return reflect.DeepEqual(normalize(a), normalize(b)) }

func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

func lengthOf(v any) (int, bool) {
	switch t := v.(type) {
	case string:
		return utf8.RuneCountInString(t), true
	case []any:
		return len(t), true
	case map[string]any:
		return len(t), true
	}
	return 0, false
}

func containsValue(container, want any) (bool, error) {
	switch c := container.(type) {
	case string:
		w, ok := want.(string)
		if !ok {
			return false, fmt.Errorf("a string can only contain a string")
		}
		return strings.Contains(c, w), nil
	case []any:
		for _, x := range c {
			if equalJSON(x, want) {
				return true, nil
			}
		}
		return false, nil
	}
	return false, fmt.Errorf("%s does not support contains", typeName(container))
}

func show(v any) string {
	b, err := json.Marshal(v)
	s := string(b)
	if err != nil {
		s = fmt.Sprint(v)
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}
