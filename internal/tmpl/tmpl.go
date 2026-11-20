// Package tmpl parses the {{...}} substitution syntax used in suite files.
//
//	{{name}}            variable (input, capture, scenario var, matrix var)
//	{{$random_hex 8}}   builtin call; name starts with '$', args are space separated
//
// There are no expressions: only variables and a small fixed set of builtins.
package tmpl

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Part is a literal, a variable reference or a builtin call.
type Part struct {
	Lit  string
	Var  string
	Func string // without the '$'
	Args []string
}

// Builtins lists the supported builtin functions and their argument counts.
var Builtins = map[string]int{
	"random_hex":          1, // N random hex characters
	"file_base64":         1, // fixture file contents, base64
	"file_sha256_b64":     1, // SHA-256 of a fixture file, base64
	"file_text":           1, // fixture file contents as UTF-8 text
	"base64_of":           1, // base64 of a variable's text value (a variable name, not a literal)
	"utc_now_minus_hours": 1, // current UTC time minus N hours, as YYYY-MM-DDTHH:MM:SSZ
}

// Parse splits s into parts. Unterminated "{{" is an error.
func Parse(s string) ([]Part, error) {
	var parts []Part
	for {
		i := strings.Index(s, "{{")
		if i < 0 {
			if s != "" {
				parts = append(parts, Part{Lit: s})
			}
			return parts, nil
		}
		if i > 0 {
			parts = append(parts, Part{Lit: s[:i]})
		}
		j := strings.Index(s[i:], "}}")
		if j < 0 {
			return nil, fmt.Errorf("unterminated {{ in %q", s)
		}
		expr := strings.TrimSpace(s[i+2 : i+j])
		if expr == "" {
			return nil, fmt.Errorf("empty {{}} in %q", s)
		}
		if strings.HasPrefix(expr, "$") {
			f := strings.Fields(expr[1:])
			n, ok := Builtins[f[0]]
			if !ok {
				return nil, fmt.Errorf("unknown builtin $%s", f[0])
			}
			if len(f)-1 != n {
				return nil, fmt.Errorf("builtin $%s takes %d argument(s), got %d", f[0], n, len(f)-1)
			}
			parts = append(parts, Part{Func: f[0], Args: f[1:]})
		} else {
			if strings.ContainsAny(expr, " \t") {
				return nil, fmt.Errorf("invalid variable name %q", expr)
			}
			parts = append(parts, Part{Var: expr})
		}
		s = s[i+j+2:]
	}
}

// Strings calls fn for every string found in v (maps, slices, strings and
// YAML nodes).
func Strings(v any, fn func(string) error) error {
	switch t := v.(type) {
	case string:
		return fn(t)
	case []any:
		for _, x := range t {
			if err := Strings(x, fn); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, x := range t {
			if err := Strings(x, fn); err != nil {
				return err
			}
		}
	case map[any]any:
		for _, x := range t {
			if err := Strings(x, fn); err != nil {
				return err
			}
		}
	case *yaml.Node:
		if t == nil {
			return nil
		}
		if t.Kind == yaml.ScalarNode && t.Tag == "!!str" {
			return fn(t.Value)
		}
		for _, c := range t.Content {
			if err := Strings(c, fn); err != nil {
				return err
			}
		}
	}
	return nil
}
