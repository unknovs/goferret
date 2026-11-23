package suite

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/unknovs/goferret/internal/jsonptr"
	"github.com/unknovs/goferret/internal/tmpl"
)

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
var nameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

// Validate statically checks the whole loaded suite (not just a selection)
// without contacting any service. It returns every problem found.
func (s *Suite) Validate() []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	tags := map[string]bool{}
	for _, t := range s.Tags {
		if tags[t] {
			add("suite: duplicate tag %q", t)
		}
		tags[t] = true
	}
	for _, t := range s.FullRunOnlyTags {
		if !tags[t] {
			add("suite: fullRunOnlyTags names undeclared tag %q", t)
		}
	}
	for name, svc := range s.Services {
		if svc.BaseURL == "" {
			add("service %s: baseUrl is required", name)
		}
		s.checkTemplates(&errs, "service "+name, svc.BaseURL)
		for _, h := range svc.Headers {
			s.checkTemplates(&errs, "service "+name, h)
		}
		for _, h := range svc.OptionalHeaders {
			s.checkTemplates(&errs, "service "+name, h)
		}
	}

	// Variables visible everywhere: inputs, then setup captures.
	global := map[string]bool{}
	for n := range s.Contract.Inputs {
		global[n] = true
	}
	setupScope := copySet(global)
	s.validateSteps(&errs, "setup", s.Setup, setupScope, true)
	for _, st := range s.Setup {
		for n := range st.Capture {
			global[n] = true
		}
	}

	ids := map[string]*Scenario{}
	for _, sc := range s.Loaded {
		if !idRe.MatchString(sc.ID) {
			add("scenario %q: id must match %s", sc.ID, idRe)
		}
		if prev, dup := ids[sc.ID]; dup {
			add("duplicate scenario id %s (in %s and %s)", sc.ID, prev.Source, sc.Source)
		}
		ids[sc.ID] = sc
		for _, t := range sc.Tags {
			if !tags[t] {
				add("scenario %s: tag %q is not declared in suite.yaml tags", sc.ID, t)
			}
		}
	}
	for _, sc := range s.Loaded {
		for _, d := range sc.DependsOn {
			if _, ok := ids[d]; !ok {
				add("scenario %s: dependsOn unknown scenario %q", sc.ID, d)
			}
		}
	}
	if cyc := findCycle(s.Loaded, ids); cyc != "" {
		add("dependency cycle: %s", cyc)
	}

	for _, sc := range s.Loaded {
		scope := copySet(global)
		for _, d := range s.dependencyClosure(sc, ids) {
			for _, e := range ids[d].Exports {
				scope[e] = true
			}
		}
		for k := range sc.Vars {
			scope[k] = true
		}
		for k := range sc.MatrixVars {
			scope[k] = true
		}
		for k, v := range sc.Vars {
			_ = tmpl.Strings(v, func(str string) error {
				s.refCheck(&errs, "scenario "+sc.ID+" var "+k, str, scope)
				return nil
			})
		}
		if len(sc.Steps) == 0 {
			add("scenario %s: has no steps", sc.ID)
		}
		s.validateSteps(&errs, "scenario "+sc.ID, sc.Steps, scope, false)
		s.validateSteps(&errs, "scenario "+sc.ID+" cleanup", sc.Cleanup, scope, false)
		for _, e := range sc.Exports {
			if !scope[e] {
				add("scenario %s: exports %q which is never defined", sc.ID, e)
			}
		}
	}
	return errs
}

func (s *Suite) dependencyClosure(sc *Scenario, ids map[string]*Scenario) []string {
	seen := map[string]bool{}
	var out []string
	var walk func(*Scenario)
	walk = func(x *Scenario) {
		for _, d := range x.DependsOn {
			if seen[d] || ids[d] == nil {
				continue
			}
			seen[d] = true
			out = append(out, d)
			walk(ids[d])
		}
	}
	walk(sc)
	return out
}

func findCycle(all []*Scenario, ids map[string]*Scenario) string {
	const white, grey, black = 0, 1, 2
	color := map[string]int{}
	var stack []string
	var visit func(id string) string
	visit = func(id string) string {
		color[id] = grey
		stack = append(stack, id)
		for _, d := range ids[id].DependsOn {
			if ids[d] == nil {
				continue
			}
			if color[d] == grey {
				return strings.Join(append(append([]string{}, stack...), d), " -> ")
			}
			if color[d] == white {
				if c := visit(d); c != "" {
					return c
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
		return ""
	}
	for _, sc := range all {
		if color[sc.ID] == white {
			if c := visit(sc.ID); c != "" {
				return c
			}
		}
	}
	return ""
}

func copySet(m map[string]bool) map[string]bool {
	o := make(map[string]bool, len(m))
	for k, v := range m {
		o[k] = v
	}
	return o
}

func (s *Suite) validateSteps(errs *[]error, where string, steps []Step, scope map[string]bool, isSetup bool) {
	add := func(format string, a ...any) { *errs = append(*errs, fmt.Errorf(format, a...)) }
	seen := map[string]bool{}
	for i := range steps {
		st := &steps[i]
		w := fmt.Sprintf("%s step %q", where, st.ID)
		if !nameRe.MatchString(st.ID) {
			add("%s: id must match %s", where, nameRe)
			continue
		}
		if seen[st.ID] {
			add("%s: duplicate step id %s", where, st.ID)
		}
		seen[st.ID] = true
		if _, ok := s.Services[st.Service]; !ok {
			add("%s: unknown service %q", w, st.Service)
		}
		if st.DelaySeconds < 0 || st.DelaySeconds > 300 {
			add("%s: delaySeconds must be between 0 and 300", w)
		}
		switch st.Method {
		case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodHead:
		default:
			add("%s: unsupported method %q", w, st.Method)
		}
		if !strings.HasPrefix(st.Path, "/") && !strings.HasPrefix(st.Path, "{{") {
			add("%s: path must start with '/'", w)
		}
		bodies := 0
		for _, b := range []bool{st.JSON != nil, st.Body != "", len(st.Form) > 0, st.Multipart != nil} {
			if b {
				bodies++
			}
		}
		if bodies > 1 {
			add("%s: json, body, form and multipart are mutually exclusive", w)
		}
		// set: runs before the request, so its names are visible to the request itself
		for k, v := range st.Set {
			if !nameRe.MatchString(k) {
				add("%s: invalid set name %q", w, k)
			}
			_ = tmpl.Strings(v, func(str string) error { s.refCheck(errs, w+" set "+k, str, scope); return nil })
		}
		for k := range st.Set {
			scope[k] = true
		}
		for _, v := range st.Form {
			s.refCheck(errs, w, v, scope)
		}
		if st.Multipart != nil {
			for field, p := range st.Multipart.Files {
				if _, err := s.FixturePath(p); err != nil {
					add("%s: multipart file %s: %v", w, field, err)
				}
			}
			for _, v := range st.Multipart.Fields {
				s.refCheck(errs, w, v, scope)
			}
		}
		// references in the request part use variables known before this step
		s.refCheck(errs, w, st.Path, scope)
		for _, v := range st.Query {
			s.refCheck(errs, w, v, scope)
		}
		for _, v := range st.Headers {
			s.refCheck(errs, w, v, scope)
		}
		s.refCheck(errs, w, st.Body, scope)
		_ = tmpl.Strings(st.JSON, func(str string) error { s.refCheck(errs, w, str, scope); return nil })

		if st.Poll != nil {
			if st.Method != http.MethodGet {
				add("%s: poll is only allowed on GET steps (never retry mutating calls)", w)
			}
			if st.Poll.Attempts < 1 || st.Poll.Attempts > 120 || st.Poll.IntervalSeconds < 1 {
				add("%s: poll needs attempts 1-120 and intervalSeconds >= 1", w)
			}
			if len(st.Poll.Until) == 0 {
				add("%s: poll needs at least one 'until' check", w)
			}
			for _, c := range st.Poll.Until {
				s.checkCheck(errs, w+" poll", c, scope)
			}
		}
		// Captures run before assertions, so assertions may reference them.
		for name := range st.Capture {
			scope[name] = true
		}
		names := map[string]bool{}
		for _, g := range st.Assert {
			if g.Name == "" {
				add("%s: assertion group without a name", w)
			}
			if names[g.Name] {
				add("%s: duplicate assertion group name %q", w, g.Name)
			}
			names[g.Name] = true
			if len(g.Checks) == 0 {
				add("%s: assertion group %q has no checks", w, g.Name)
			}
			for _, c := range g.Checks {
				s.checkCheck(errs, fmt.Sprintf("%s group %q", w, g.Name), c, scope)
			}
		}
		for name, c := range st.Capture {
			if !nameRe.MatchString(name) {
				add("%s: invalid capture name %q", w, name)
			}
			n := 0
			if c.JSON != "" {
				n++
			}
			if c.Header != "" {
				n++
			}
			if c.Status {
				n++
			}
			if c.JSON != "" {
				if err := jsonptr.Valid(c.JSON); err != nil {
					add("%s capture %s: %v", w, name, err)
				}
			}
			if n != 1 {
				add("%s capture %s: exactly one of json, header, status is required", w, name)
			}
		}
		// captures become visible to later steps
		for name := range st.Capture {
			scope[name] = true
		}
	}
}

func (s *Suite) checkCheck(errs *[]error, where string, c Check, scope map[string]bool) {
	add := func(format string, a ...any) { *errs = append(*errs, fmt.Errorf(format, a...)) }
	if c.ResponseTimeLtMs != nil {
		if c.Status != nil || c.Header != "" || c.JSON != "" || c.DecodeJSON != nil {
			add("%s: responseTimeLtMs is a target of its own", where)
		}
		return
	}
	if c.DecodeJSON != nil {
		if err := jsonptr.Valid(*c.DecodeJSON); err != nil {
			add("%s: decodeJson: %v", where, err)
		}
		if c.Status != nil || c.Header != "" {
			add("%s: decodeJson only applies to JSON targets", where)
		}
	}
	// Target: status, header, body text, or JSON pointer (the default; "" is
	// the root), optionally inside a variable's value (var).
	if c.Body && (c.Status != nil || c.Header != "" || c.JSON != "" || c.Var != "" || c.DecodeJSON != nil) {
		add("%s: body is a target of its own", where)
	}
	if c.Status != nil && (c.Header != "" || c.JSON != "" || c.Var != "") {
		add("%s: a check has exactly one target (status, header, body, var or json)", where)
	}
	if c.Header != "" && (c.JSON != "" || c.Var != "") {
		add("%s: a check has exactly one target (status, header, body, var or json)", where)
	}
	if c.Var != "" {
		s.refCheck(errs, where, "{{"+c.Var+"}}", scope)
	}
	if c.Status == nil && c.Header == "" && !c.Body {
		if err := jsonptr.Valid(c.JSON); err != nil {
			add("%s: %v", where, err)
		}
	}
	ops := 0
	for _, b := range []bool{c.Exists != nil, c.Absent != nil, c.Type != "", c.Eq.Kind != 0, c.EqJSON != "", c.NeJSON != "", c.Ne.Kind != 0, c.Matches != "", c.Min != nil, c.Max != nil, c.Length != nil, c.LengthMin != nil, c.Empty != nil, c.Contains.Kind != 0, c.In.Kind != 0, c.Any != nil, c.All != nil} {
		if b {
			ops++
		}
	}
	if c.Status != nil {
		ops-- // status: N is itself an equality
		if ops > 0 {
			add("%s: status takes no other operators", where)
		}
		return
	}
	if ops == 0 {
		add("%s: check has no operator", where)
	}
	if c.NeJSON != "" {
		if err := jsonptr.Valid(c.NeJSON); err != nil {
			add("%s: neJson: %v", where, err)
		}
	}
	if c.EqJSON != "" {
		if err := jsonptr.Valid(c.EqJSON); err != nil {
			add("%s: eqJson: %v", where, err)
		}
		if c.Status != nil || c.Header != "" {
			add("%s: eqJson only applies to JSON targets", where)
		}
	}
	switch c.Type {
	case "", "string", "number", "boolean", "array", "object", "null":
	default:
		add("%s: unknown type %q", where, c.Type)
	}
	if c.Matches != "" {
		if _, err := regexp.Compile(c.Matches); err != nil {
			// a template may be inside; only compile-check static patterns
			if !strings.Contains(c.Matches, "{{") {
				add("%s: invalid regular expression: %v", where, err)
			}
		}
		s.refCheck(errs, where, c.Matches, scope)
	}
	if c.In.Kind != 0 && c.In.Kind != yaml.SequenceNode {
		add("%s: in needs a list of values", where)
	}
	for _, n := range []*yaml.Node{&c.Eq, &c.Ne, &c.Contains, &c.In} {
		if n != nil {
			_ = tmpl.Strings(n, func(str string) error { s.refCheck(errs, where, str, scope); return nil })
		}
	}
	if c.Any != nil {
		s.checkCheck(errs, where+" (any)", *c.Any, scope)
	}
	if c.All != nil {
		s.checkCheck(errs, where+" (all)", *c.All, scope)
	}
}

// refCheck verifies every variable referenced in str is known, and that
// fixture paths in builtins stay under the fixture root.
func (s *Suite) refCheck(errs *[]error, where, str string, scope map[string]bool) {
	parts, err := tmpl.Parse(str)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %v", where, err))
		return
	}
	for _, p := range parts {
		if p.Var != "" && !scope[p.Var] {
			*errs = append(*errs, fmt.Errorf("%s: unknown variable {{%s}} (not an input, capture, scenario var or export visible here)", where, p.Var))
		}
		if p.Func == "base64_of" && !scope[p.Args[0]] {
			*errs = append(*errs, fmt.Errorf("%s: $base64_of %s: unknown variable", where, p.Args[0]))
		}
		if strings.HasPrefix(p.Func, "file_") {
			if _, err := s.FixturePath(p.Args[0]); err != nil {
				*errs = append(*errs, fmt.Errorf("%s: %v", where, err))
			}
		}
	}
}

func (s *Suite) checkTemplates(errs *[]error, where, str string) {
	scope := map[string]bool{}
	for n := range s.Contract.Inputs {
		scope[n] = true
	}
	s.refCheck(errs, where, str, scope)
}

// ReferencedInputs returns the declared inputs a scenario (or the setup
// steps, when sc is nil) needs, including those used by the services it
// calls. Used for selected-scenario preflight.
func (s *Suite) ReferencedInputs(steps []Step, vars map[string]any) []string {
	found := map[string]bool{}
	note := func(str string) {
		parts, err := tmpl.Parse(str)
		if err != nil {
			return
		}
		for _, p := range parts {
			if p.Var != "" {
				if _, ok := s.Contract.Inputs[p.Var]; ok {
					found[p.Var] = true
				}
			}
			if p.Func == "base64_of" {
				if _, ok := s.Contract.Inputs[p.Args[0]]; ok {
					found[p.Args[0]] = true
				}
			}
		}
	}
	services := map[string]bool{}
	for _, st := range steps {
		services[st.Service] = true
		note(st.Path)
		note(st.Body)
		for _, v := range st.Query {
			note(v)
		}
		for _, v := range st.Headers {
			note(v)
		}
		_ = tmpl.Strings(st.JSON, func(x string) error { note(x); return nil })
		for _, v := range st.Form {
			note(v)
		}
		for _, v := range st.Set {
			_ = tmpl.Strings(v, func(x string) error { note(x); return nil })
		}
		if st.Multipart != nil {
			for _, v := range st.Multipart.Fields {
				note(v)
			}
		}
		for _, g := range st.Assert {
			for _, c := range g.Checks {
				for _, n := range []*yaml.Node{&c.Eq, &c.Ne, &c.Contains} {
					_ = tmpl.Strings(n, func(x string) error { note(x); return nil })
				}
				note(c.Matches)
			}
		}
	}
	for _, v := range vars {
		_ = tmpl.Strings(v, func(x string) error { note(x); return nil })
	}
	for name := range services {
		svc, ok := s.Services[name]
		if !ok {
			continue
		}
		note(svc.BaseURL)
		for _, h := range svc.Headers {
			note(h)
		}
		// optional headers do not make an input required, but the input is
		// still "referenced" so it is resolved when present.
	}
	var out []string
	for n := range found {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// OptionalHeaderInputs returns inputs referenced only by optional headers of
// the services the steps use (resolved when present, never required).
func (s *Suite) OptionalHeaderInputs(steps []Step) []string {
	found := map[string]bool{}
	for _, st := range steps {
		svc := s.Services[st.Service]
		for _, h := range svc.OptionalHeaders {
			parts, err := tmpl.Parse(h)
			if err != nil {
				continue
			}
			for _, p := range parts {
				if _, ok := s.Contract.Inputs[p.Var]; ok && p.Var != "" {
					found[p.Var] = true
				}
			}
		}
	}
	var out []string
	for n := range found {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
