package suite

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/unknovs/goferret/internal/config"
)

type scenarioFile struct {
	Version   int         `yaml:"version"`
	Scenarios []*Scenario `yaml:"scenarios"`
}

func strictDecode(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	return nil
}

// Load reads suite.yaml, its input contract and all included scenario files,
// and expands matrices. It does not contact any service.
func Load(path string) (*Suite, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	var s Suite
	if err := strictDecode(abs, &s); err != nil {
		return nil, err
	}
	if s.Version != 1 {
		return nil, fmt.Errorf("%s: unsupported version %d (want 1)", path, s.Version)
	}
	s.Path = abs
	s.Dir = filepath.Dir(abs)
	if s.Inputs == "" {
		return nil, fmt.Errorf("%s: 'inputs' (path to inputs.yaml) is required", path)
	}
	if s.Contract, err = config.LoadContract(filepath.Join(s.Dir, s.Inputs)); err != nil {
		return nil, err
	}
	for _, rel := range s.Scenarios {
		p := filepath.Join(s.Dir, rel)
		var f scenarioFile
		if err := strictDecode(p, &f); err != nil {
			return nil, err
		}
		if f.Version != 1 {
			return nil, fmt.Errorf("%s: unsupported version %d (want 1)", p, f.Version)
		}
		for _, sc := range f.Scenarios {
			sc.Source = rel
			expanded, err := expand(sc)
			if err != nil {
				return nil, fmt.Errorf("%s: %v", p, err)
			}
			s.Loaded = append(s.Loaded, expanded...)
		}
	}
	return &s, nil
}

// expand turns a scenario with a matrix into one scenario per entry.
func expand(sc *Scenario) ([]*Scenario, error) {
	if len(sc.Matrix) == 0 {
		return []*Scenario{sc}, nil
	}
	var out []*Scenario
	for i, m := range sc.Matrix {
		suffix, _ := m["suffix"].(string)
		if suffix == "" {
			return nil, fmt.Errorf("scenario %s: matrix entry %d needs a string 'suffix'", sc.ID, i+1)
		}
		cp := *sc
		cp.ID = sc.ID + "." + suffix
		cp.Matrix = nil
		cp.MatrixVars = map[string]any{}
		for k, v := range m {
			cp.MatrixVars[k] = v
		}
		out = append(out, &cp)
	}
	return out, nil
}

// FixturePath resolves a fixture path under the fixture root and rejects
// anything that escapes it.
func (s *Suite) FixturePath(rel string) (string, error) {
	if strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "\\") || filepath.VolumeName(rel) != "" || filepath.IsAbs(rel) {
		return "", fmt.Errorf("fixture path %q must be relative to the fixture root", rel)
	}
	root := s.FixtureRoot
	if root == "" {
		root = "."
	}
	rootAbs, err := filepath.Abs(filepath.Join(s.Dir, root))
	if err != nil {
		return "", err
	}
	p := filepath.Join(rootAbs, filepath.FromSlash(rel))
	r, err := filepath.Rel(rootAbs, p)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("fixture path %q escapes the fixture root", rel)
	}
	return p, nil
}

// ScenarioByID finds an expanded scenario.
func (s *Suite) ScenarioByID(id string) *Scenario {
	for _, sc := range s.Loaded {
		if sc.ID == id {
			return sc
		}
	}
	return nil
}
