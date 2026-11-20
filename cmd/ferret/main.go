// Command ferret runs file-driven HTTP regression suites.
//
//	ferret run      --tests SUITE.yaml [--env-file F] [--tag T]... [--exclude-tag T]... [--scenario ID]... --output DIR [--debug]
//	ferret validate --tests SUITE.yaml [--env-file F]
//	ferret list     --tests SUITE.yaml [--tag T]...
//
// Exit status: 0 all selected scenarios passed; 1 an assertion failed, a step
// errored or a dependency blocked a scenario; 2 invalid definitions,
// configuration or infrastructure failure.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"

	"github.com/unknovs/goferret/internal/config"
	"github.com/unknovs/goferret/internal/docs"
	"github.com/unknovs/goferret/internal/engine"
	"github.com/unknovs/goferret/internal/report"
	"github.com/unknovs/goferret/internal/results"
	"github.com/unknovs/goferret/internal/suite"
)

// Version is set at build time: -ldflags "-X main.Version=1.2.3".
var Version = "dev"

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func main() { os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr)) }

func realMain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:], stdout, stderr)
	case "validate":
		return cmdValidate(args[1:], stdout, stderr)
	case "list":
		return cmdList(args[1:], stdout, stderr)
	case "docs":
		return cmdDocs(args[1:], stdout, stderr)
	case "report":
		return cmdReport(args[1:], stdout, stderr)
	case "version", "--version":
		_, _ = fmt.Fprintln(stdout, "ferret", Version)
		return 0
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	}
	_, _ = fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
	usage(stderr)
	return 2
}

func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `usage:
  ferret run      --tests SUITE.yaml --output DIR [--env-file FILE] [--tag T] [--exclude-tag T] [--scenario ID] [--debug] [--log-file FILE] [--save-files DIR]
  ferret validate --tests SUITE.yaml [--env-file FILE]
  ferret list     --tests SUITE.yaml [--tag T] [--exclude-tag T]
  ferret docs     --tests SUITE.yaml [--readme FILE] [--check]
  ferret report   --input results.json [--output report.html]

The suite path can also be given in the TESTS environment variable; --tests wins.
`)
}

type common struct {
	tests         string
	envFiles      multi
	tags, exclude multi
	scenarios     multi
}

func (c *common) register(fs *flag.FlagSet, withEnv bool) {
	fs.StringVar(&c.tests, "tests", "", "path to suite.yaml (or set TESTS)")
	if withEnv {
		fs.Var(&c.envFiles, "env-file", "explicit dotenv file (repeatable; later files override earlier ones)")
	}
	fs.Var(&c.tags, "tag", "run only scenarios with this tag (repeatable, union)")
	fs.Var(&c.exclude, "exclude-tag", "skip scenarios with this tag (repeatable)")
	fs.Var(&c.scenarios, "scenario", "run only this scenario id (repeatable)")
}

func (c *common) suitePath() (string, error) {
	p := c.tests
	if p == "" {
		p = os.Getenv("TESTS")
	}
	if p == "" {
		return "", errors.New("no suite given: pass --tests SUITE.yaml or set TESTS")
	}
	return p, nil
}

func loadSuite(c *common) (*suite.Suite, error) {
	p, err := c.suitePath()
	if err != nil {
		return nil, err
	}
	return suite.Load(p)
}

// readEnvFiles loads the explicit dotenv files in order. A later file overrides
// an earlier one per input: setting X replaces any earlier X or X_FILE.
func readEnvFiles(paths []string, contract *config.Contract) (map[string]string, error) {
	var merged map[string]string
	for _, p := range paths {
		dot, err := config.ReadDotenv(p)
		if err != nil {
			return nil, err
		}
		if err := contract.CheckDotenvKeys(dot, p); err != nil {
			return nil, err
		}
		if merged == nil {
			merged = map[string]string{}
		}
		for k, v := range dot {
			base := strings.TrimSuffix(k, "_FILE")
			delete(merged, base)
			delete(merged, base+"_FILE")
			merged[k] = v
		}
	}
	return merged, nil
}

func printErrs(w io.Writer, errs []error) {
	for _, e := range errs {
		_, _ = fmt.Fprintf(w, "  - %v\n", e)
	}
}

func cmdValidate(args []string, stdout, stderr io.Writer) int {
	var c common
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c.register(fs, true)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	s, err := loadSuite(&c)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	if errs := s.Validate(); len(errs) > 0 {
		_, _ = fmt.Fprintf(stderr, "suite %s is invalid (%d problem(s)):\n", s.Name, len(errs))
		printErrs(stderr, errs)
		return 2
	}
	if _, err := readEnvFiles(c.envFiles, s.Contract); err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	_, _ = fmt.Fprintf(stdout, "suite %s is valid: %d scenarios, %d setup steps\n", s.Name, len(s.Loaded), len(s.Setup))
	return 0
}

func cmdReport(args []string, stdout, stderr io.Writer) int {
	var in, out string
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&in, "input", "", "results.json written by a previous run")
	fs.StringVar(&out, "output", "", "HTML file to write (default: report.html next to the input)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if in == "" {
		_, _ = fmt.Fprintln(stderr, "error: --input is required")
		return 2
	}
	if out == "" {
		out = filepath.Join(filepath.Dir(in), "report.html")
	}
	res, err := report.Build(in, out)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	_, _ = fmt.Fprintf(stdout, "report written to %s", res.Output)
	if res.Attachments > 0 {
		_, _ = fmt.Fprintf(stdout, " (%d attachment(s) referenced, %d copied)", res.Attachments, res.Copied)
	}
	_, _ = fmt.Fprintln(stdout)
	if len(res.Missing) > 0 {
		_, _ = fmt.Fprintf(stderr, "warning: %d referenced attachment(s) are missing next to %s and are shown as missing in the report\n", len(res.Missing), in)
	}
	return 0
}

func cmdDocs(args []string, stdout, stderr io.Writer) int {
	var c common
	var readme string
	var check bool
	fs := flag.NewFlagSet("docs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c.register(fs, false)
	fs.StringVar(&readme, "readme", "", "README to update (default: README.md next to suite.yaml)")
	fs.BoolVar(&check, "check", false, "do not write; exit 1 if the README is out of date")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	s, err := loadSuite(&c)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	if errs := s.Validate(); len(errs) > 0 {
		_, _ = fmt.Fprintf(stderr, "suite %s is invalid (%d problem(s)):\n", s.Name, len(errs))
		printErrs(stderr, errs)
		return 2
	}
	if readme == "" {
		readme = filepath.Join(s.Dir, "README.md")
	}
	old, err := os.ReadFile(readme)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	updated, err := docs.Replace(string(old), docs.Generate(s))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %s: %v\n", readme, err)
		return 2
	}
	if updated == string(old) {
		_, _ = fmt.Fprintf(stdout, "%s is up to date\n", readme)
		return 0
	}
	if check {
		_, _ = fmt.Fprintf(stderr, "%s is out of date: run `ferret docs --tests %s`\n", readme, filepath.ToSlash(c.suiteArg()))
		return 1
	}
	if err := os.WriteFile(readme, []byte(updated), 0o644); err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	_, _ = fmt.Fprintf(stdout, "updated %s\n", readme)
	return 0
}

func (c *common) suiteArg() string {
	p, _ := c.suitePath()
	return p
}

func cmdList(args []string, stdout, stderr io.Writer) int {
	var c common
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c.register(fs, false)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	s, err := loadSuite(&c)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	sel, err := engine.Select(s, engine.Selection{Tags: c.tags, ExcludeTags: c.exclude, Scenarios: c.scenarios})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	req, opt := engine.NeededInputs(s, sel)
	sort.Strings(req)
	for _, x := range sel {
		groups := 0
		for _, st := range x.S.Steps {
			groups += len(st.Assert)
		}
		dep := ""
		if x.ByDependency {
			dep = "  (by dependency)"
		}
		_, _ = fmt.Fprintf(stdout, "%-40s tags=[%s] steps=%d assertions=%d%s\n", x.S.ID, strings.Join(x.S.Tags, ","), len(x.S.Steps), groups, dep)
	}
	_, _ = fmt.Fprintf(stdout, "\nrequired inputs: %s\n", strings.Join(req, ", "))
	if len(opt) > 0 {
		sort.Strings(opt)
		_, _ = fmt.Fprintf(stdout, "optional inputs: %s\n", strings.Join(opt, ", "))
	}
	return 0
}

func cmdRun(args []string, stdout, stderr io.Writer) int {
	var c common
	var out, rerun, logFile, saveDir string
	var debug bool
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c.register(fs, true)
	fs.StringVar(&out, "output", "", "output directory for results.json and junit.xml; the previous run's own files in it are removed first")
	fs.StringVar(&rerun, "rerun-failed", "", "results.json of a previous run: run again only the scenarios that did not pass")
	fs.BoolVar(&debug, "debug", false, "print the step-by-step log to the console: each scenario with its steps and calls, then numbered assertions (bodies only for a step that failed)")
	fs.StringVar(&logFile, "log-file", "", "also write that log to this file (no log file is written unless you pass this)")
	fs.StringVar(&saveDir, "save-files", "", "save documents returned by the services (PDF, ZIP, ...) as real files under this directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if out == "" {
		_, _ = fmt.Fprintln(stderr, "error: --output is required")
		return 2
	}
	if rerun != "" {
		prev, err := results.Load(rerun)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
			return 2
		}
		ids := prev.FailedScenarioIDs()
		if len(ids) == 0 {
			_, _ = fmt.Fprintf(stderr, "error: %s has no failed, errored or blocked scenarios to rerun\n", rerun)
			return 2
		}
		c.scenarios = append(c.scenarios, ids...)
		_, _ = fmt.Fprintf(stdout, "rerunning %d scenario(s) from %s\n", len(ids), rerun)
	}
	// From here on this run owns the output directory: the previous run's own files
	// are removed first (after --rerun-failed has read them), so a run that fails
	// early can never leave an old results.json to be mistaken for the current one.
	if err := results.PrepareOutputDir(out); err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	s, err := loadSuite(&c)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	if errs := s.Validate(); len(errs) > 0 {
		_, _ = fmt.Fprintf(stderr, "suite %s is invalid (%d problem(s)):\n", s.Name, len(errs))
		printErrs(stderr, errs)
		return 2
	}
	filter := engine.Selection{Tags: c.tags, ExcludeTags: c.exclude, Scenarios: c.scenarios}
	sel, err := engine.Select(s, filter)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	if len(sel) == 0 {
		_, _ = fmt.Fprintln(stderr, "error: the selection matches no scenarios")
		return 2
	}

	// Preflight: resolve only the inputs the selection needs.
	dot, err := readEnvFiles(c.envFiles, s.Contract)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	required, optional := engine.NeededInputs(s, sel)
	sort.Strings(required)
	values, errs := s.Contract.Resolve(required, config.Sources{Env: os.LookupEnv, Dotenv: dot})
	optVals, optErrs := s.Contract.Resolve(optional, config.Sources{Env: os.LookupEnv, Dotenv: dot})
	errs = append(errs, optErrs...) // an optional input that is set but invalid is still an error
	if len(errs) > 0 {
		_, _ = fmt.Fprintln(stderr, "configuration problems (no requests were sent):")
		printErrs(stderr, errs)
		return 2
	}
	for k, v := range optVals {
		values[k] = v
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	opts := engine.Options{OutDir: out, Progress: stdout, SaveDir: saveDir}
	// The log goes to the console (--debug) and/or the file (--log-file), nowhere else.
	var logs []io.Writer
	if debug {
		logs = append(logs, stdout)
	}
	if logFile != "" {
		f, err := os.Create(logFile)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "error: cannot write the log file: %v\n", err)
			return 2
		}
		defer func() { _ = f.Close() }()
		logs = append(logs, f)
	}
	if len(logs) > 0 {
		opts.Debug = io.MultiWriter(logs...)
	}
	r := engine.New(s, values, opts)
	res := r.Run(ctx, sel, filter)

	if err := results.WriteAll(out, res); err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	printSummary(stdout, res, out)
	if res.AllPassed() {
		return 0
	}
	return 1
}

// printFailures lists every scenario that did not pass, with its failing steps
// and the exact failing checks, so a failure is visible without opening results.json.
func printFailures(w io.Writer, r *results.Results) {
	const maxMsg = 400
	first := true
	for _, sc := range r.Scenarios {
		if sc.Status == results.Passed {
			continue
		}
		if first {
			_, _ = fmt.Fprintln(w, "\nFailures:")
			first = false
		}
		_, _ = fmt.Fprintf(w, "\n  %s [%s]\n", sc.ID, sc.Status)
		if sc.Reason != "" {
			_, _ = fmt.Fprintf(w, "    reason: %s\n", sc.Reason)
		}
		for _, st := range sc.Steps {
			if st.Status == results.Passed {
				continue
			}
			_, _ = fmt.Fprintf(w, "    step %s [%s]\n", st.ID, st.Status)
			if st.Reason != "" {
				_, _ = fmt.Fprintf(w, "      reason: %s\n", st.Reason)
			}
			for _, g := range st.Groups {
				if g.Status == results.Passed {
					continue
				}
				_, _ = fmt.Fprintf(w, "      - %s [%s]\n", g.Name, g.Status)
				for _, c := range g.Checks {
					if c.Status == results.Passed {
						continue
					}
					msg := c.Message
					if len(msg) > maxMsg {
						msg = msg[:maxMsg] + "..."
					}
					_, _ = fmt.Fprintf(w, "          %s: %s\n", c.Desc, msg)
				}
			}
		}
	}
}

func printSummary(w io.Writer, r *results.Results, out string) {
	printFailures(w, r)
	a, s, su := r.Totals.Assertions, r.Totals.Scenarios, r.Totals.SetupAssertions
	_, _ = fmt.Fprintf(w, "\nAssertions: total %d | passed %d | failed %d | errored %d | blocked %d\n", a.Total, a.Passed, a.Failed, a.Errored, a.Blocked)
	_, _ = fmt.Fprintf(w, "Scenarios:  total %d | passed %d | failed %d | errored %d | blocked %d\n", s.Total, s.Passed, s.Failed, s.Errored, s.Blocked)
	if su.Total > 0 {
		_, _ = fmt.Fprintf(w, "Setup checks (not counted as regression assertions): %d, passed %d\n", su.Total, su.Passed)
	}
	_, _ = fmt.Fprintf(w, "HTTP calls: %d (including setup and helper calls)\n", r.Totals.Calls)
	if !r.Run.Selection.Complete {
		_, _ = fmt.Fprintf(w, "Partial run: %d of %d scenarios selected (+%d by dependency); this is not full coverage.\n",
			r.Run.Selection.Selected, r.Run.Selection.Available, r.Run.Selection.ByDependency)
	}
	_, _ = fmt.Fprintf(w, "Results written to %s (results.json, junit.xml)\n", out)
}
