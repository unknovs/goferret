package engine

import (
	"fmt"
	"strings"

	"github.com/unknovs/goferret/internal/results"
	"github.com/unknovs/goferret/internal/suite"
)

// The log (--debug, or --log-file) is printed live, scenario by scenario:
//
//	---<scenario id>----
//	----step.<step id>  METHOD /path
//	     call #N METHOD url -> status (ms)
//	     (only for a step that did not pass: problem, request body, response body)
//	----step.<next step> ...
//	-----Assertions
//	 1  [step] assertion name  PASS|FAIL|SKIP
//	      - the exact check and why it failed
//
// The normal one-line summary of the scenario follows the block. Every line
// passes through the redactor.

func (r *Runner) logf(format string, a ...any) {
	if r.Debug != nil {
		_, _ = fmt.Fprintln(r.Debug, r.Redact.String(fmt.Sprintf(format, a...)))
	}
}

func (r *Runner) traceScenarioStart(id string) {
	if r.Debug == nil {
		return
	}
	r.logf("")
	r.logf("---%s----", id)
}

func (r *Runner) traceStepStart(st suite.Step, cleanup bool) {
	if r.Debug == nil {
		return
	}
	kind := ""
	switch {
	case cleanup:
		kind = "  (cleanup)"
	case st.Helper:
		kind = "  (helper)"
	}
	r.logf("----step.%s  %s %s%s", st.ID, st.Method, st.Path, kind)
}

func (r *Runner) traceBlocked(st suite.Step, reason string) {
	if r.Debug == nil {
		return
	}
	r.logf("----step.%s  %s %s  NOT SENT: %s", st.ID, st.Method, st.Path, reason)
}

// traceCall prints one recorded HTTP call under its step.
func (r *Runner) traceCall(c results.Call) {
	if r.Debug == nil {
		return
	}
	status := "no response"
	if c.Status > 0 {
		status = fmt.Sprintf("%d", c.Status)
	}
	att := ""
	if c.Attempt > 0 {
		att = fmt.Sprintf("  attempt %d", c.Attempt)
	}
	r.logf("     call #%d %s %s -> %s (%d ms)%s", c.Seq, c.Method, c.URL, status, c.DurationMs, att)
	if c.Error != "" {
		r.logf("          error: %s", c.Error)
	}
	for _, p := range c.SavedFiles {
		r.logf("          saved file: %s", p)
	}
}

// traceFailedStep adds the evidence for a step that did not pass: why, and the
// bodies of its calls. Bodies are shown only here, never for passing steps.
func (r *Runner) traceFailedStep(st results.StepResult) {
	if r.Debug == nil {
		return
	}
	if st.Reason != "" {
		r.logf("     problem: %s", st.Reason)
	}
	for _, seq := range st.CallSeqs {
		if seq < 1 || seq > len(r.calls) {
			continue
		}
		c := r.calls[seq-1]
		r.logf("     call #%d request body (%d bytes):%s", c.Seq, c.ReqBody.Bytes, bodyText(c.ReqBody.Preview, c.ReqBody.Omitted))
		r.logf("     call #%d response body (%d bytes):%s", c.Seq, c.ResBody.Bytes, bodyText(c.ResBody.Preview, c.ResBody.Omitted))
		if c.ResBody.Truncated {
			r.logf("          (response body cut at %d bytes)", PreviewBytes)
		}
		if c.ReqBody.Truncated {
			r.logf("          (request body cut at %d bytes)", PreviewBytes)
		}
	}
}

func bodyText(preview, omitted string) string {
	switch {
	case preview != "":
		return " " + preview
	case omitted != "":
		return " (" + omitted + ")"
	}
	return " (empty)"
}

// traceAssertions prints every named assertion of the scenario, numbered in
// step order, with the exact failing check under each one that did not pass.
func (r *Runner) traceAssertions(sr results.ScenarioResult) {
	if r.Debug == nil {
		return
	}
	n := 0
	for _, st := range sr.Steps {
		if len(st.Groups) > 0 && n == 0 {
			r.logf("-----Assertions")
		}
		for _, g := range st.Groups {
			n++
			word := map[string]string{results.Passed: "PASS", results.Failed: "FAIL", results.Errored: "ERROR", results.Blocked: "SKIP"}[g.Status]
			r.logf("%3d  [%s] %s  %s", n, st.ID, g.Name, word)
			for _, c := range g.Checks {
				if c.Status != results.Passed {
					r.logf("        - %s: %s", c.Desc, strings.TrimSpace(c.Message))
				}
			}
			if g.Status == results.Blocked && st.Reason != "" {
				r.logf("        - %s", st.Reason)
			}
		}
	}
}
