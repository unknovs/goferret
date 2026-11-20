package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/unknovs/goferret/internal/config"
	"github.com/unknovs/goferret/internal/jsonptr"
	"github.com/unknovs/goferret/internal/results"
	"github.com/unknovs/goferret/internal/suite"
)

// Options configure a run.
type Options struct {
	OutDir   string
	Debug    io.Writer // the step-by-step log, may be nil
	SaveDir  string    // save returned documents here as real files (--save-files), may be empty
	Progress io.Writer
}

// New prepares a runner. Secret inputs are registered with the redactor
// before any request is built.
func New(s *suite.Suite, inputs map[string]config.Value, opt Options) *Runner {
	r := &Runner{Suite: s, Inputs: inputs, OutDir: opt.OutDir, Debug: opt.Debug, SaveDir: opt.SaveDir, Progress: opt.Progress, Redact: &Redactor{}}
	r.R = &Renderer{Suite: s}
	r.Ev = &Evaluator{R: r.R}
	timeout := 60 * time.Second
	if s.TimeoutSeconds > 0 {
		timeout = time.Duration(s.TimeoutSeconds) * time.Second
	}
	r.Client = newClient()
	r.Timeout = timeout
	r.root = NewScope(nil)
	for name, v := range inputs {
		r.root.Set(name, Var{Value: v.Value, Secret: v.Secret})
		if v.Secret {
			r.Redact.AddSecret(v.Value)
		}
	}
	r.shared = NewScope(r.root)
	r.exported = map[string]string{}
	return r
}

// Run executes setup and the selected scenarios in suite order.
func (r *Runner) Run(ctx context.Context, sel []Selected, filter Selection) *results.Results {
	start := time.Now()
	res := &results.Results{Version: results.SchemaVersion, Tool: "ferret"}
	res.Run.Suite = r.Suite.Name
	res.Run.StartedAt = start.UTC()
	res.Run.Selection = results.Selection{
		Tags: filter.Tags, ExcludeTags: filter.ExcludeTags, Scenarios: filter.Scenarios,
		Complete: !filter.Filtered(), Available: len(r.Suite.Loaded),
	}
	for _, x := range sel {
		if x.ByDependency {
			res.Run.Selection.ByDependency++
		} else {
			res.Run.Selection.Selected++
		}
	}

	status := map[string]string{}
	setupOK := true
	if len(r.Suite.Setup) > 0 {
		r.traceScenarioStart("setup")
		sr := r.runSteps(ctx, "setup", "setup", r.Suite.Setup, nil, r.shared)
		sr.Kind = "setup"
		sr.Description = "suite setup"
		res.Scenarios = append(res.Scenarios, sr)
		setupOK = sr.Status == results.Passed
		r.progress(sr)
	}
	for _, x := range sel {
		var sr results.ScenarioResult
		r.traceScenarioStart(x.S.ID)
		ran := false
		switch dep := firstBadDependency(x.S, status); {
		case !setupOK:
			sr = r.blockedScenario(x, "suite setup did not pass")
		case dep != "":
			sr = r.blockedScenario(x, fmt.Sprintf("dependency %s did not pass (%s)", dep, status[dep]))
		case ctx.Err() != nil:
			sr = r.blockedScenario(x, "run cancelled")
		default:
			ran = true
			sc := NewScope(r.shared)
			sr = r.runScenarioBody(ctx, x, sc)
		}
		if !ran {
			r.traceAssertions(sr) // a blocked scenario still lists its assertions, as SKIP
		}
		status[x.S.ID] = sr.Status
		res.Scenarios = append(res.Scenarios, sr)
		r.progress(sr)
	}
	res.Calls = r.calls
	r.sweep(res)
	res.Run.FinishedAt = time.Now().UTC()
	res.Run.DurationMs = time.Since(start).Milliseconds()
	res.Compute()
	return res
}

func firstBadDependency(sc *suite.Scenario, status map[string]string) string {
	for _, d := range sc.DependsOn {
		if status[d] != results.Passed {
			return d
		}
	}
	return ""
}

func (r *Runner) runScenarioBody(ctx context.Context, x Selected, sc *Scope) results.ScenarioResult {
	s := x.S
	for k, v := range s.MatrixVars {
		sc.Set(k, Var{Value: v})
	}
	keys := make([]string, 0, len(s.Vars))
	for k := range s.Vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v, secret, err := r.renderVar(s.Vars[k], sc)
		if err != nil {
			sr := r.blockedScenario(x, fmt.Sprintf("var %s: %v", k, err))
			sr.Status = results.Errored
			return sr
		}
		sc.Set(k, Var{Value: v, Secret: secret})
		if secret {
			r.Redact.AddSecret(fmt.Sprint(v))
		}
	}
	sr := r.runSteps(ctx, s.ID, "regression", s.Steps, s.Cleanup, sc)
	sr.Description = s.Description
	sr.Tags = s.Tags
	sr.IncludedByDependency = x.ByDependency
	if sr.Status == results.Passed {
		for _, name := range s.Exports {
			v, ok := sc.Get(name)
			if !ok {
				continue
			}
			if other, dup := r.exported[name]; dup {
				sr.Status, sr.Reason = results.Errored, fmt.Sprintf("export %q already exported by scenario %s", name, other)
				continue
			}
			r.exported[name] = s.ID
			r.shared.Set(name, v)
		}
	}
	return sr
}

func (r *Runner) renderVar(v any, sc *Scope) (any, bool, error) {
	if s, ok := v.(string); ok {
		return r.R.value(s, sc)
	}
	out, err := r.R.Any(v, sc)
	return out, false, err
}

// runSteps runs steps in order, then cleanup. After the first step that is
// not passed, the remaining steps are blocked; cleanup always runs.
func (r *Runner) runSteps(ctx context.Context, id, kind string, steps, cleanup []suite.Step, sc *Scope) results.ScenarioResult {
	start := time.Now()
	sr := results.ScenarioResult{ID: id, Kind: kind, Status: results.Passed}
	blockedBy := ""
	for _, st := range steps {
		if blockedBy != "" {
			reason := fmt.Sprintf("blocked: step %s did not pass", blockedBy)
			r.traceBlocked(st, reason)
			sr.Steps = append(sr.Steps, r.blockedStep(id, st, false, reason))
			continue
		}
		if ctx.Err() != nil {
			r.traceBlocked(st, "run cancelled")
			sr.Steps = append(sr.Steps, r.blockedStep(id, st, false, "run cancelled"))
			continue
		}
		r.traceStepStart(st, false)
		stepRes := r.runStep(ctx, id, st, sc, false)
		if stepRes.Status != results.Passed {
			r.traceFailedStep(stepRes)
		}
		sr.Steps = append(sr.Steps, stepRes)
		if stepRes.Status != results.Passed {
			blockedBy = st.ID
		}
		sr.Status = worse(sr.Status, stepRes.Status)
	}
	for _, st := range cleanup {
		r.traceStepStart(st, true)
		stepRes := r.runStep(ctx, id, st, sc, true)
		if stepRes.Status == results.Errored && len(stepRes.CallSeqs) == 0 && strings.Contains(stepRes.Reason, "is not defined") {
			// Nothing to clean up: the step that creates the resource never ran,
			// so this is not a failure of its own.
			stepRes = r.blockedStep(id, st, true, "cleanup skipped: "+stepRes.Reason)
		}
		if stepRes.Status != results.Passed {
			r.traceFailedStep(stepRes)
		}
		sr.Steps = append(sr.Steps, stepRes)
		if stepRes.Status != results.Passed && stepRes.Status != results.Blocked && sr.Status == results.Passed {
			// a cleanup failure never replaces an earlier failure
			sr.Status, sr.Reason = results.Errored, "cleanup step "+st.ID+" did not pass"
		}
	}
	if sr.Status != results.Passed && sr.Reason == "" {
		sr.Reason = "step " + blockedBy + " did not pass"
	}
	sr.DurationMs = time.Since(start).Milliseconds()
	r.traceAssertions(sr)
	return sr
}

// worse orders statuses: errored > failed > passed.
func worse(a, b string) string {
	rank := map[string]int{results.Passed: 0, results.Blocked: 1, results.Failed: 2, results.Errored: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func (r *Runner) blockedScenario(x Selected, reason string) results.ScenarioResult {
	sr := results.ScenarioResult{ID: x.S.ID, Kind: "regression", Description: x.S.Description, Tags: x.S.Tags,
		Status: results.Blocked, Reason: reason, IncludedByDependency: x.ByDependency}
	for _, st := range x.S.Steps {
		r.traceBlocked(st, reason)
		sr.Steps = append(sr.Steps, r.blockedStep(x.S.ID, st, false, reason))
	}
	return sr
}

func (r *Runner) blockedStep(scID string, st suite.Step, cleanup bool, reason string) results.StepResult {
	sr := results.StepResult{ID: st.ID, Cleanup: cleanup, Helper: st.Helper, Status: results.Blocked, Reason: reason}
	seen := map[string]int{}
	for _, g := range st.Assert {
		sr.Groups = append(sr.Groups, results.GroupResult{ID: groupID(scID, st.ID, g.Name, seen), Name: g.Name, Source: g.Source, Status: results.Blocked})
	}
	return sr
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func groupID(scID, stepID, name string, seen map[string]int) string {
	slug := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(slug) > 60 {
		slug = slug[:60]
	}
	seen[slug]++
	if seen[slug] > 1 {
		slug = fmt.Sprintf("%s-%d", slug, seen[slug])
	}
	return scID + "/" + stepID + "/" + slug
}

func (r *Runner) runStep(ctx context.Context, scID string, st suite.Step, sc *Scope, cleanup bool) results.StepResult {
	res := results.StepResult{ID: st.ID, Cleanup: cleanup, Helper: st.Helper, Status: results.Passed}
	// set: assigns variables before the request is built
	setNames := make([]string, 0, len(st.Set))
	for k := range st.Set {
		setNames = append(setNames, k)
	}
	sort.Strings(setNames)
	for _, k := range setNames {
		v, secret, err := r.renderVar(st.Set[k], sc)
		if err != nil {
			res.Status, res.Reason = results.Errored, r.Redact.String("set "+k+": "+err.Error())
			return r.erroredGroups(res, scID, st)
		}
		sc.Set(k, Var{Value: v, Secret: secret})
		if secret {
			r.Redact.AddSecret(fmt.Sprint(v))
		}
	}
	attempts := 1
	if st.Poll != nil {
		attempts = st.Poll.Attempts
	}
	var resp *Response
	var err error
	for a := 1; a <= attempts; a++ {
		label := 0
		if st.Poll != nil {
			label = a
		}
		before := r.seq
		resp, err = r.do(ctx, scID, st, sc, label)
		if r.seq > before {
			res.CallSeqs = append(res.CallSeqs, r.seq)
		}
		if st.Poll == nil {
			break
		}
		if err == nil && r.pollSatisfied(st, resp, sc) {
			break
		}
		if a < attempts {
			select {
			case <-ctx.Done():
				a = attempts
			case <-time.After(time.Duration(st.Poll.IntervalSeconds) * time.Second):
			}
		} else {
			if err == nil {
				err = fmt.Errorf("readiness condition not reached after %d attempts", attempts)
				resp = nil
			}
		}
	}

	seen := map[string]int{}
	if err != nil || resp == nil {
		msg := "no response received"
		if err != nil {
			msg = err.Error()
		}
		res.Status, res.Reason = results.Errored, r.Redact.String(msg)
		for _, g := range st.Assert {
			res.Groups = append(res.Groups, results.GroupResult{ID: groupID(scID, st.ID, g.Name, seen), Name: g.Name, Source: g.Source, Status: results.Errored,
				Checks: []results.CheckResult{{Desc: "response", Status: results.Errored, Message: res.Reason}}})
		}
		return res
	}

	// Captures first, so a failed assertion does not hide a capture problem.
	names := make([]string, 0, len(st.Capture))
	for n := range st.Capture {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c := st.Capture[n]
		v, cerr := capture(c, resp)
		if cerr != nil {
			res.Status = results.Errored
			res.Reason = strings.TrimSpace(res.Reason + " capture " + n + ": " + r.Redact.String(cerr.Error()) + ";")
			continue
		}
		sc.Set(n, Var{Value: v, Secret: c.Secret})
		if c.Secret {
			if s, ok := v.(string); ok {
				r.Redact.AddSecret(s)
			} else if b, merr := json.Marshal(v); merr == nil {
				r.Redact.AddSecret(string(b))
			}
		}
		res.Captured = append(res.Captured, n)
	}

	for _, g := range st.Assert {
		gr := results.GroupResult{ID: groupID(scID, st.ID, g.Name, seen), Name: g.Name, Source: g.Source, Status: results.Passed}
		for _, c := range g.Checks {
			for _, cr := range r.Ev.Run(c, resp, sc) {
				cr.Message = r.Redact.String(cr.Message)
				cr.Desc = r.Redact.String(cr.Desc)
				gr.Checks = append(gr.Checks, cr)
				gr.Status = worse(gr.Status, cr.Status)
			}
		}
		res.Groups = append(res.Groups, gr)
		res.Status = worse(res.Status, gr.Status)
	}
	return res
}

func (r *Runner) erroredGroups(res results.StepResult, scID string, st suite.Step) results.StepResult {
	seen := map[string]int{}
	for _, g := range st.Assert {
		res.Groups = append(res.Groups, results.GroupResult{ID: groupID(scID, st.ID, g.Name, seen), Name: g.Name, Source: g.Source, Status: results.Errored,
			Checks: []results.CheckResult{{Desc: "step", Status: results.Errored, Message: res.Reason}}})
	}
	return res
}

func (r *Runner) pollSatisfied(st suite.Step, resp *Response, sc *Scope) bool {
	for _, c := range st.Poll.Until {
		for _, cr := range r.Ev.Run(c, resp, sc) {
			if cr.Status != results.Passed {
				return false
			}
		}
	}
	return true
}

func capture(c suite.Capture, resp *Response) (any, error) {
	switch {
	case c.Status:
		return float64(resp.Status), nil
	case c.Header != "":
		v := resp.Header.Values(c.Header)
		if len(v) == 0 {
			return nil, fmt.Errorf("response has no header %s", c.Header)
		}
		return v[0], nil
	default:
		doc, err := resp.JSON()
		if err != nil {
			return nil, err
		}
		v, found, perr := jsonptr.Get(doc, c.JSON)
		if perr != nil {
			return nil, perr
		}
		if !found {
			return nil, fmt.Errorf("no value at %s in the response", c.JSON)
		}
		return v, nil
	}
}

func (r *Runner) progress(sr results.ScenarioResult) {
	if r.Progress == nil {
		return
	}
	tag := map[string]string{results.Passed: "PASS   ", results.Failed: "FAIL   ", results.Errored: "ERROR  ", results.Blocked: "BLOCKED"}[sr.Status]
	groups, bad := 0, 0
	for _, st := range sr.Steps {
		for _, g := range st.Groups {
			groups++
			if g.Status != results.Passed {
				bad++
			}
		}
	}
	_, _ = fmt.Fprintf(r.Progress, "%s %-40s %d steps, %d assertions, %.1fs\n", tag, sr.ID, len(sr.Steps), groups, float64(sr.DurationMs)/1000)
	if sr.Status == results.Passed {
		return
	}
	if sr.Reason != "" {
		_, _ = fmt.Fprintf(r.Progress, "        %s\n", sr.Reason)
	}
	if r.Debug != nil {
		return // the log above already listed every failing check; do not repeat it
	}
	for _, st := range sr.Steps {
		if st.Status == results.Blocked {
			continue
		}
		if st.Status != results.Passed && st.Reason != "" {
			_, _ = fmt.Fprintf(r.Progress, "        step %s: %s\n", st.ID, st.Reason)
		}
		for _, g := range st.Groups {
			if g.Status == results.Passed || g.Status == results.Blocked {
				continue
			}
			for _, c := range g.Checks {
				if c.Status != results.Passed {
					_, _ = fmt.Fprintf(r.Progress, "        %s: [%s] %s -> %s\n", st.ID, g.Name, c.Desc, c.Message)
				}
			}
		}
	}
}
