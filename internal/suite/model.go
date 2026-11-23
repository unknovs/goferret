// Package suite defines the YAML test-definition model and loads it strictly.
package suite

import (
	"gopkg.in/yaml.v3"

	"github.com/unknovs/goferret/internal/config"
)

// Suite is suite.yaml plus everything it includes.
type Suite struct {
	Version        int      `yaml:"version"`
	Name           string   `yaml:"name"`
	Inputs         string   `yaml:"inputs"`      // path to inputs.yaml, relative to suite.yaml
	FixtureRoot    string   `yaml:"fixtureRoot"` // relative to suite.yaml; fixtures may not escape it
	TimeoutSeconds int      `yaml:"timeoutSeconds"`
	Tags           []string `yaml:"tags"`
	// FullRunOnlyTags mark scenarios that only make sense in a complete run (for
	// example the final statistics markers). A filtered run skips them unless
	// they are selected explicitly by --scenario or by their tag.
	FullRunOnlyTags []string           `yaml:"fullRunOnlyTags"`
	Services        map[string]Service `yaml:"services"`
	Setup           []Step             `yaml:"setup"`
	Scenarios       []string           `yaml:"scenarios"` // scenario files, relative to suite.yaml

	// Filled by Load.
	Dir      string           `yaml:"-"`
	Path     string           `yaml:"-"`
	Contract *config.Contract `yaml:"-"`
	Loaded   []*Scenario      `yaml:"-"`
}

// Service is a base URL with default headers.
type Service struct {
	BaseURL string `yaml:"baseUrl"`
	// Headers are sent on every request to the service. A header whose value
	// references an unset input is an error.
	Headers map[string]string `yaml:"headers"`
	// OptionalHeaders are omitted when a referenced input is not set.
	OptionalHeaders map[string]string `yaml:"optionalHeaders"`
}

// Scenario is an ordered, independent workflow.
type Scenario struct {
	ID          string           `yaml:"id"`
	Description string           `yaml:"description"`
	Tags        []string         `yaml:"tags"`
	DependsOn   []string         `yaml:"dependsOn"`
	Exports     []string         `yaml:"exports"` // variable names made available to dependents
	Vars        map[string]any   `yaml:"vars"`    // rendered once at scenario start
	Matrix      []map[string]any `yaml:"matrix"`  // each entry needs a "suffix"; produces one scenario per entry
	Steps       []Step           `yaml:"steps"`
	Cleanup     []Step           `yaml:"cleanup"`
	Source      string           `yaml:"-"`

	// MatrixVars is set on expanded scenarios.
	MatrixVars map[string]any `yaml:"-"`
}

// Step is one HTTP request with captures and assertions.
type Step struct {
	ID             string             `yaml:"id"`
	Description    string             `yaml:"description"`
	Service        string             `yaml:"service"`
	Method         string             `yaml:"method"`
	Path           string             `yaml:"path"`
	Query          map[string]string  `yaml:"query"`
	Headers        map[string]string  `yaml:"headers"`
	NoService      bool               `yaml:"noServiceHeaders"` // skip the service's default headers
	JSON           any                `yaml:"json"`             // JSON body; "{{var}}" values keep their type
	Form           map[string]string  `yaml:"form"`             // application/x-www-form-urlencoded body
	Multipart      *Multipart         `yaml:"multipart"`        // multipart/form-data body with fixture files
	Set            map[string]any     `yaml:"set"`              // variables assigned before the request is built
	TimeoutSeconds int                `yaml:"timeoutSeconds"`   // overrides the suite timeout for this step
	DelaySeconds   int                `yaml:"delaySeconds"`     // wait this long before sending the request (for APIs that order by a coarse timestamp)
	Body           string             `yaml:"body"`             // text body
	Capture        map[string]Capture `yaml:"capture"`
	Assert         []Group            `yaml:"assert"`
	Poll           *Poll              `yaml:"poll"`
	Helper         bool               `yaml:"helper"` // a support call (fetching a token, preparing data); its checks are counted apart from regression assertions
}

// Multipart describes a multipart/form-data body.
type Multipart struct {
	Files  map[string]string `yaml:"files"`  // form field -> fixture path
	Fields map[string]string `yaml:"fields"` // form field -> text value
}

// Poll repeats a step until a condition holds, with a hard attempt limit.
// Only intended for readiness checks; mutating calls must not use it.
type Poll struct {
	Attempts        int `yaml:"attempts"`
	IntervalSeconds int `yaml:"intervalSeconds"`
	// Until is satisfied when all checks pass on a response.
	Until []Check `yaml:"until"`
}

// Capture extracts one value from the response. Exactly one source.
type Capture struct {
	JSON   string `yaml:"json"`   // JSON pointer into the response body
	Header string `yaml:"header"` // response header name
	Status bool   `yaml:"status"` // the status code
	Secret bool   `yaml:"secret"` // value is redacted from all output
}

// Group is a named assertion group (a set of checks reported as one result).
type Group struct {
	Name   string  `yaml:"name"`
	Source string  `yaml:"source"` // optional free-text reference, for example to an original test; not used when running
	Checks []Check `yaml:"checks"`
}

// Check is one or more conditions on a single target: the status, a header
// or a JSON pointer in the body. Every operator present must hold.
type Check struct {
	Status *int   `yaml:"status"`
	JSON   string `yaml:"json"`
	Header string `yaml:"header"`

	Exists    *bool     `yaml:"exists"`
	Absent    *bool     `yaml:"absent"`
	Type      string    `yaml:"type"` // string number boolean array object null
	Eq        yaml.Node `yaml:"eq"`
	EqJSON    string    `yaml:"eqJson"` // equal to the value at another JSON pointer in the same response
	NeJSON    string    `yaml:"neJson"` // differs from the value at another JSON pointer in the same response
	Ne        yaml.Node `yaml:"ne"`
	Matches   string    `yaml:"matches"`
	Min       *float64  `yaml:"min"`
	Max       *float64  `yaml:"max"`
	Length    *int      `yaml:"length"`
	LengthMin *int      `yaml:"lengthMin"`
	Empty     *bool     `yaml:"empty"`
	Contains  yaml.Node `yaml:"contains"`
	In        yaml.Node `yaml:"in"` // equals one of the listed values
	// Any requires at least one array element to satisfy the nested check
	// (the check's own json pointer is relative to the element).
	Any *Check `yaml:"any"`
	// All requires every array element to satisfy the nested check (an empty array passes).
	All *Check `yaml:"all"`
	// DecodeJSON: the value at json is base64 text of a JSON document; the
	// operators apply to the value at this pointer inside the decoded document.
	DecodeJSON *string `yaml:"decodeJson"`
	// Var makes the value of a variable the target (a value captured earlier,
	// possibly by another step); json then points inside that value.
	Var string `yaml:"var"`
	// Body makes the raw response text the target.
	Body bool `yaml:"body"`
	// ResponseTimeLtMs is a target of its own: response time below N ms.
	ResponseTimeLtMs *int `yaml:"responseTimeLtMs"`
}
