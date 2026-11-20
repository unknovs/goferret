package results

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// OutputFiles are the only names a run owns in its output directory.
var OutputFiles = []string{"results.json", "junit.xml", "report.html"}

// legacyAttachments is the folder older versions wrote bodies into.
const legacyAttachments = "attachments"

// PrepareOutputDir creates the output directory and removes the previous run's
// own files from it (OutputFiles and the legacy attachments folder), and
// nothing else. A run that fails before writing results therefore leaves no
// stale results.json that could be reported as the current run.
func PrepareOutputDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range OutputFiles {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("cannot remove the previous %s from %s: %v", name, dir, err)
		}
	}
	if err := os.RemoveAll(filepath.Join(dir, legacyAttachments)); err != nil {
		return fmt.Errorf("cannot remove the previous %s folder from %s: %v", legacyAttachments, dir, err)
	}
	return nil
}

// WriteAll writes results.json and junit.xml.
func WriteAll(dir string, r *Results) error {
	r.Compute()
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "results.json"), data, 0o644); err != nil {
		return fmt.Errorf("write results.json: %v", err)
	}
	j, err := JUnit(r)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "junit.xml"), j, 0o644); err != nil {
		return fmt.Errorf("write junit.xml: %v", err)
	}
	return nil
}

type juSuites struct {
	XMLName  xml.Name  `xml:"testsuites"`
	Name     string    `xml:"name,attr"`
	Tests    int       `xml:"tests,attr"`
	Failures int       `xml:"failures,attr"`
	Errors   int       `xml:"errors,attr"`
	Skipped  int       `xml:"skipped,attr"`
	Suites   []juSuite `xml:"testsuite"`
}

type juSuite struct {
	Name     string   `xml:"name,attr"`
	Tests    int      `xml:"tests,attr"`
	Failures int      `xml:"failures,attr"`
	Errors   int      `xml:"errors,attr"`
	Skipped  int      `xml:"skipped,attr"`
	Time     string   `xml:"time,attr"`
	Cases    []juCase `xml:"testcase"`
}

type juCase struct {
	Name      string  `xml:"name,attr"`
	ClassName string  `xml:"classname,attr"`
	Failure   *juMsg  `xml:"failure,omitempty"`
	Error     *juMsg  `xml:"error,omitempty"`
	Skipped   *juSkip `xml:"skipped,omitempty"`
}

type juMsg struct {
	Message string `xml:"message,attr"`
	Text    string `xml:",chardata"`
}

type juSkip struct {
	Message string `xml:"message,attr"`
}

// JUnit renders one testsuite per scenario and one testcase per named
// assertion group. Failures, errors and blocked (skipped) stay distinct.
func JUnit(r *Results) ([]byte, error) {
	root := juSuites{Name: r.Run.Suite}
	for _, sc := range r.Scenarios {
		su := juSuite{Name: sc.ID, Time: fmt.Sprintf("%.3f", float64(sc.DurationMs)/1000)}
		add := func(c juCase) {
			su.Cases = append(su.Cases, c)
			su.Tests++
			switch {
			case c.Failure != nil:
				su.Failures++
			case c.Error != nil:
				su.Errors++
			case c.Skipped != nil:
				su.Skipped++
			}
		}
		for _, st := range sc.Steps {
			class := sc.ID + "." + st.ID
			if len(st.Groups) == 0 && st.Status != Passed {
				// a step that failed before any assertion ran still shows up
				c := juCase{Name: "step " + st.ID, ClassName: class}
				fill(&c, st.Status, st.Reason, st.Reason)
				add(c)
			}
			for _, g := range st.Groups {
				c := juCase{Name: g.Name, ClassName: class}
				var msgs []string
				for _, ch := range g.Checks {
					if ch.Status != Passed {
						msgs = append(msgs, ch.Desc+": "+ch.Message)
					}
				}
				msg := strings.Join(msgs, "\n")
				if msg == "" {
					msg = st.Reason
				}
				if msg == "" {
					msg = sc.Reason
				}
				fill(&c, g.Status, firstLine(msg), msg)
				add(c)
			}
		}
		if len(sc.Steps) == 0 {
			c := juCase{Name: "scenario", ClassName: sc.ID}
			fill(&c, sc.Status, sc.Reason, sc.Reason)
			add(c)
		}
		root.Tests += su.Tests
		root.Failures += su.Failures
		root.Errors += su.Errors
		root.Skipped += su.Skipped
		root.Suites = append(root.Suites, su)
	}
	out, err := xml.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), out...), nil
}

func fill(c *juCase, status, short, long string) {
	switch status {
	case Failed:
		c.Failure = &juMsg{Message: short, Text: long}
	case Errored:
		c.Error = &juMsg{Message: short, Text: long}
	case Blocked:
		c.Skipped = &juSkip{Message: short}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Load reads a results.json written by a previous run and checks its version.
func Load(path string) (*Results, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Results
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("%s: not valid results JSON: %v", path, err)
	}
	if r.Version != SchemaVersion {
		return nil, fmt.Errorf("%s: unsupported results version %d (this build reads %d)", path, r.Version, SchemaVersion)
	}
	return &r, nil
}

// FailedScenarioIDs lists regression scenarios that did not pass (failed,
// errored or blocked), in file order.
func (r *Results) FailedScenarioIDs() []string {
	var ids []string
	for _, sc := range r.Scenarios {
		if sc.Kind == "regression" && sc.Status != Passed && !sc.IncludedByDependency {
			ids = append(ids, sc.ID)
		}
	}
	return ids
}
