// Package results defines the versioned results.json model and its writers.
package results

import "time"

// SchemaVersion is the results.json format version.
const SchemaVersion = 1

// Statuses.
const (
	Passed  = "passed"
	Failed  = "failed"
	Errored = "errored"
	Blocked = "blocked"
)

// Results is the canonical, already-sanitized run record.
type Results struct {
	Version   int              `json:"version"`
	Tool      string           `json:"tool"`
	Run       RunInfo          `json:"run"`
	Totals    Totals           `json:"totals"`
	Scenarios []ScenarioResult `json:"scenarios"`
	Calls     []Call           `json:"calls"`
}

// RunInfo describes the run and its selection.
type RunInfo struct {
	Suite      string    `json:"suite"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	DurationMs int64     `json:"durationMs"`
	Selection  Selection `json:"selection"`
}

// Selection records what was asked for. Complete is true only for an
// unfiltered run, so a partial run never claims full coverage.
type Selection struct {
	Tags         []string `json:"tags,omitempty"`
	ExcludeTags  []string `json:"excludeTags,omitempty"`
	Scenarios    []string `json:"scenarios,omitempty"`
	Complete     bool     `json:"complete"`
	Available    int      `json:"availableScenarios"`
	Selected     int      `json:"selectedScenarios"`
	ByDependency int      `json:"includedByDependency"`
}

// Counts of outcomes.
type Counts struct {
	Total   int `json:"total"`
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Errored int `json:"errored"`
	Blocked int `json:"blocked"`
}

// Totals are derived from the records below, never maintained separately.
type Totals struct {
	Assertions      Counts `json:"assertions"`      // regression assertion groups only
	SetupAssertions Counts `json:"setupAssertions"` // setup, helper and cleanup checks, reported separately
	Scenarios       Counts `json:"scenarios"`
	Calls           int    `json:"httpCalls"`
}

// ScenarioResult is one scenario (Kind "setup" for suite setup).
type ScenarioResult struct {
	ID                   string       `json:"id"`
	Kind                 string       `json:"kind"` // "regression" or "setup"
	Description          string       `json:"description,omitempty"`
	Tags                 []string     `json:"tags,omitempty"`
	Status               string       `json:"status"`
	Reason               string       `json:"reason,omitempty"`
	IncludedByDependency bool         `json:"includedByDependency,omitempty"`
	DurationMs           int64        `json:"durationMs"`
	Steps                []StepResult `json:"steps"`
}

// StepResult is one HTTP step (or a cleanup step).
type StepResult struct {
	ID       string        `json:"id"`
	Cleanup  bool          `json:"cleanup,omitempty"`
	Helper   bool          `json:"helper,omitempty"`
	Status   string        `json:"status"`
	Reason   string        `json:"reason,omitempty"`
	CallSeqs []int         `json:"calls,omitempty"`
	Captured []string      `json:"captured,omitempty"` // capture names only; values stay out of the file
	Groups   []GroupResult `json:"assertions"`
}

// GroupResult is one named assertion group.
type GroupResult struct {
	ID     string        `json:"id"`
	Name   string        `json:"name"`
	Source string        `json:"source,omitempty"`
	Status string        `json:"status"`
	Checks []CheckResult `json:"checks"`
}

// CheckResult is one condition inside a group.
type CheckResult struct {
	Desc    string `json:"desc"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// Call is one recorded HTTP exchange (including failed attempts).
type Call struct {
	Seq        int               `json:"seq"`
	Scenario   string            `json:"scenario"`
	Step       string            `json:"step"`
	Service    string            `json:"service"`
	Attempt    int               `json:"attempt,omitempty"`
	Method     string            `json:"method"`
	URL        string            `json:"url"`
	ReqHeaders map[string]string `json:"requestHeaders,omitempty"`
	ReqBody    Body              `json:"requestBody"`
	Status     int               `json:"status,omitempty"`
	ResHeaders map[string]string `json:"responseHeaders,omitempty"`
	ResBody    Body              `json:"responseBody"`
	SavedFiles []string          `json:"savedFiles,omitempty"` // documents from this response saved because of --save-files
	DurationMs int64             `json:"durationMs"`
	Error      string            `json:"error,omitempty"`
}

// Body is a bounded preview plus an optional attachment for the full
// (redacted) content.
type Body struct {
	Bytes      int    `json:"bytes"`
	Preview    string `json:"preview,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	Attachment string `json:"attachment,omitempty"` // path relative to the output directory
	Omitted    string `json:"omitted,omitempty"`    // reason the content was not written
}

// Compute derives Totals from the scenario records.
func (r *Results) Compute() {
	r.normalize()
	var t Totals
	for _, sc := range r.Scenarios {
		if sc.Kind == "regression" {
			bump(&t.Scenarios, sc.Status)
		}
		for _, st := range sc.Steps {
			for _, g := range st.Groups {
				if sc.Kind == "regression" && !st.Helper && !st.Cleanup {
					bump(&t.Assertions, g.Status)
				} else {
					bump(&t.SetupAssertions, g.Status)
				}
			}
		}
	}
	t.Calls = len(r.Calls)
	r.Totals = t
}

// normalize makes every list a JSON array (never null) so consumers need no
// null handling.
func (r *Results) normalize() {
	if r.Scenarios == nil {
		r.Scenarios = []ScenarioResult{}
	}
	if r.Calls == nil {
		r.Calls = []Call{}
	}
	for i := range r.Scenarios {
		sc := &r.Scenarios[i]
		if sc.Steps == nil {
			sc.Steps = []StepResult{}
		}
		for j := range sc.Steps {
			st := &sc.Steps[j]
			if st.Groups == nil {
				st.Groups = []GroupResult{}
			}
			for k := range st.Groups {
				if st.Groups[k].Checks == nil {
					st.Groups[k].Checks = []CheckResult{}
				}
			}
		}
	}
}

func bump(c *Counts, status string) {
	c.Total++
	switch status {
	case Passed:
		c.Passed++
	case Failed:
		c.Failed++
	case Errored:
		c.Errored++
	default:
		c.Blocked++
	}
}

// AllPassed reports whether every scenario (including setup) passed.
func (r *Results) AllPassed() bool {
	for _, sc := range r.Scenarios {
		if sc.Status != Passed {
			return false
		}
	}
	return len(r.Scenarios) > 0
}
