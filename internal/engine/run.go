// Package engine executes a loaded suite: scenarios, steps, captures,
// assertions, dependency blocking and recording.
package engine

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/unknovs/goferret/internal/config"
	"github.com/unknovs/goferret/internal/results"
	"github.com/unknovs/goferret/internal/suite"
)

// Selection filters which scenarios run.
type Selection struct {
	Tags        []string
	ExcludeTags []string
	Scenarios   []string
}

// Filtered reports whether any filter is set.
func (s Selection) Filtered() bool {
	return len(s.Tags) > 0 || len(s.ExcludeTags) > 0 || len(s.Scenarios) > 0
}

// Selected is a scenario chosen to run.
type Selected struct {
	S            *suite.Scenario
	ByDependency bool
}

// Select applies the filters in suite order and adds the dependency closure
// of every chosen scenario (marked ByDependency). Unknown scenario IDs and
// tags are errors.
func Select(s *suite.Suite, sel Selection) ([]Selected, error) {
	declared := map[string]bool{}
	for _, t := range s.Tags {
		declared[t] = true
	}
	for _, t := range append(append([]string{}, sel.Tags...), sel.ExcludeTags...) {
		if !declared[t] {
			return nil, fmt.Errorf("unknown tag %q (declared tags: %s)", t, strings.Join(s.Tags, ", "))
		}
	}
	for _, id := range sel.Scenarios {
		if s.ScenarioByID(id) == nil {
			return nil, fmt.Errorf("unknown scenario %q%s", id, suggest(s, id))
		}
	}
	chosen := map[string]bool{}
	for _, sc := range s.Loaded {
		ok := true
		if len(sel.Tags) > 0 || len(sel.Scenarios) > 0 {
			ok = false
			if hasAny(sc.Tags, sel.Tags) {
				ok = true
			}
			for _, id := range sel.Scenarios {
				if id == sc.ID {
					ok = true
				}
			}
		}
		if ok && hasAny(sc.Tags, sel.ExcludeTags) {
			ok = false
		}
		// full-run-only scenarios are skipped in any filtered run unless asked for by name or by their tag
		if ok && sel.Filtered() && hasAny(sc.Tags, s.FullRunOnlyTags) {
			explicit := hasAny(sc.Tags, intersect(sel.Tags, s.FullRunOnlyTags))
			for _, id := range sel.Scenarios {
				if id == sc.ID {
					explicit = true
				}
			}
			if !explicit {
				ok = false
			}
		}
		if ok {
			chosen[sc.ID] = true
		}
	}
	closure := map[string]bool{}
	var add func(id string)
	add = func(id string) {
		sc := s.ScenarioByID(id)
		for _, d := range sc.DependsOn {
			if !chosen[d] && !closure[d] {
				closure[d] = true
				add(d)
			}
		}
	}
	for id := range chosen {
		add(id)
	}
	var out []Selected
	for _, sc := range s.Loaded {
		switch {
		case chosen[sc.ID]:
			out = append(out, Selected{S: sc})
		case closure[sc.ID]:
			out = append(out, Selected{S: sc, ByDependency: true})
		}
	}
	return out, nil
}

func intersect(a, b []string) []string {
	var out []string
	for _, x := range a {
		for _, y := range b {
			if x == y {
				out = append(out, x)
			}
		}
	}
	return out
}

func hasAny(have, want []string) bool {
	for _, h := range have {
		for _, w := range want {
			if h == w {
				return true
			}
		}
	}
	return false
}

func suggest(s *suite.Suite, id string) string {
	var near []string
	for _, sc := range s.Loaded {
		if strings.Contains(sc.ID, id) || strings.Contains(id, sc.ID) {
			near = append(near, sc.ID)
		}
	}
	if len(near) == 0 {
		return ""
	}
	return " (did you mean " + strings.Join(near, ", ") + "?)"
}

// NeededInputs returns the inputs that must resolve for the selection (and
// the setup steps), plus inputs to resolve when present (optional headers).
func NeededInputs(s *suite.Suite, sel []Selected) (required, optional []string) {
	req := map[string]bool{}
	opt := map[string]bool{}
	steps := append([]suite.Step{}, s.Setup...)
	var vars []map[string]any
	for _, x := range sel {
		steps = append(steps, x.S.Steps...)
		steps = append(steps, x.S.Cleanup...)
		vars = append(vars, x.S.Vars)
	}
	merged := map[string]any{}
	for i, v := range vars {
		for k, val := range v {
			merged[fmt.Sprintf("%d.%s", i, k)] = val
		}
	}
	for _, n := range s.ReferencedInputs(steps, merged) {
		req[n] = true
	}
	for _, n := range s.OptionalHeaderInputs(steps) {
		opt[n] = true
	}
	for n := range req {
		required = append(required, n)
	}
	for n := range opt {
		if !req[n] {
			optional = append(optional, n)
		}
	}
	return
}

// Runner executes a suite.
type Runner struct {
	Suite    *suite.Suite
	Inputs   map[string]config.Value
	OutDir   string
	Debug    io.Writer // the step-by-step log (--debug / --log-file), may be nil
	SaveDir  string    // --save-files: returned documents are written here as real files, may be empty
	Progress io.Writer // one line per scenario, may be nil
	Redact   *Redactor
	R        *Renderer
	Ev       *Evaluator
	Client   *http.Client
	Timeout  time.Duration // default per-request timeout
	calls    []results.Call
	seq      int
	root     *Scope
	shared   *Scope
	exported map[string]string
}
