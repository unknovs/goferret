package engine

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/unknovs/goferret/internal/suite"
	"github.com/unknovs/goferret/internal/tmpl"
)

// Var is a variable value. Value keeps its JSON type for exact substitutions.
type Var struct {
	Value  any
	Secret bool
}

// Scope resolves variables: own map first, then the parent chain.
type Scope struct {
	vars   map[string]Var
	parent *Scope
}

// NewScope creates a scope with an optional parent.
func NewScope(parent *Scope) *Scope { return &Scope{vars: map[string]Var{}, parent: parent} }

// Set defines a variable in this scope.
func (s *Scope) Set(name string, v Var) { s.vars[name] = v }

// Get looks a variable up through the chain.
func (s *Scope) Get(name string) (Var, bool) {
	for c := s; c != nil; c = c.parent {
		if v, ok := c.vars[name]; ok {
			return v, true
		}
	}
	return Var{}, false
}

// Renderer substitutes {{...}} in strings and structures.
type Renderer struct {
	Suite *suite.Suite
	files map[string][]byte
}

func (rd *Renderer) readFixture(rel string) ([]byte, error) {
	p, err := rd.Suite.FixturePath(rel)
	if err != nil {
		return nil, err
	}
	if b, ok := rd.files[p]; ok {
		return b, nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("fixture %s: %v", rel, err)
	}
	if rd.files == nil {
		rd.files = map[string][]byte{}
	}
	rd.files[p] = b
	return b, nil
}

// call evaluates a builtin. The bool reports that the result is derived from a
// secret variable.
func (rd *Renderer) call(fn string, args []string, sc *Scope) (string, bool, error) {
	if fn == "base64_of" {
		v, ok := sc.Get(args[0])
		if !ok {
			return "", false, fmt.Errorf("variable {{%s}} is not defined", args[0])
		}
		t, err := scalarText(v.Value)
		if err != nil {
			return "", false, fmt.Errorf("$base64_of %s: %v", args[0], err)
		}
		return base64.StdEncoding.EncodeToString([]byte(t)), v.Secret, nil
	}
	out, err := rd.callPlain(fn, args)
	return out, false, err
}

func (rd *Renderer) callPlain(fn string, args []string) (string, error) {
	switch fn {
	case "random_hex":
		n, err := strconv.Atoi(args[0])
		if err != nil || n < 1 || n > 64 {
			return "", fmt.Errorf("$random_hex needs a length 1-64")
		}
		b := make([]byte, (n+1)/2)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		return hex.EncodeToString(b)[:n], nil
	case "file_base64":
		b, err := rd.readFixture(args[0])
		if err != nil {
			return "", err
		}
		return base64.StdEncoding.EncodeToString(b), nil
	case "file_sha256_b64":
		b, err := rd.readFixture(args[0])
		if err != nil {
			return "", err
		}
		h := sha256.Sum256(b)
		return base64.StdEncoding.EncodeToString(h[:]), nil
	case "utc_now_minus_hours":
		n, err := strconv.Atoi(args[0])
		if err != nil || n < 0 || n > 24*365 {
			return "", fmt.Errorf("$utc_now_minus_hours needs a whole number of hours 0-8760")
		}
		return time.Now().UTC().Add(-time.Duration(n) * time.Hour).Format("2006-01-02T15:04:05Z"), nil
	case "file_text":
		b, err := rd.readFixture(args[0])
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	return "", fmt.Errorf("unknown builtin $%s", fn)
}

// Str renders a string. Missing variables are errors, never empty strings.
// Non-string variable values are interpolated using their JSON text only
// when scalar; objects and arrays are rejected inside larger strings.
func (rd *Renderer) Str(s string, sc *Scope) (string, error) {
	v, _, err := rd.value(s, sc)
	if err != nil {
		return "", err
	}
	switch t := v.(type) {
	case string:
		return t, nil
	default:
		return scalarText(t)
	}
}

// value renders s. When s is exactly one {{var}} the variable's typed value
// is returned; otherwise the interpolated string.
func (rd *Renderer) value(s string, sc *Scope) (any, bool, error) {
	parts, err := tmpl.Parse(s)
	if err != nil {
		return nil, false, err
	}
	if len(parts) == 1 && parts[0].Lit == "" {
		p := parts[0]
		if p.Var != "" {
			v, ok := sc.Get(p.Var)
			if !ok {
				return nil, false, fmt.Errorf("variable {{%s}} is not defined", p.Var)
			}
			return v.Value, v.Secret, nil
		}
		out, sec, err := rd.call(p.Func, p.Args, sc)
		return out, sec, err
	}
	var b strings.Builder
	secret := false
	for _, p := range parts {
		switch {
		case p.Var != "":
			v, ok := sc.Get(p.Var)
			if !ok {
				return nil, false, fmt.Errorf("variable {{%s}} is not defined", p.Var)
			}
			secret = secret || v.Secret
			t, err := scalarText(v.Value)
			if err != nil {
				return nil, false, fmt.Errorf("variable {{%s}}: %v", p.Var, err)
			}
			b.WriteString(t)
		case p.Func != "":
			o, sec, err := rd.call(p.Func, p.Args, sc)
			if err != nil {
				return nil, false, err
			}
			secret = secret || sec
			b.WriteString(o)
		default:
			b.WriteString(p.Lit)
		}
	}
	return b.String(), secret, nil
}

func scalarText(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case bool:
		return strconv.FormatBool(t), nil
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	case int:
		return strconv.Itoa(t), nil
	case nil:
		return "null", nil
	}
	return "", fmt.Errorf("a structured value cannot be interpolated into a string; use it as the whole value")
}

// Any renders a structure: strings that are exactly one {{var}} keep the
// variable's JSON type; other strings are interpolated.
func (rd *Renderer) Any(v any, sc *Scope) (any, error) {
	switch t := v.(type) {
	case string:
		out, _, err := rd.value(t, sc)
		return out, err
	case []any:
		res := make([]any, len(t))
		for i, x := range t {
			r, err := rd.Any(x, sc)
			if err != nil {
				return nil, err
			}
			res[i] = r
		}
		return res, nil
	case map[string]any:
		res := make(map[string]any, len(t))
		for k, x := range t {
			r, err := rd.Any(x, sc)
			if err != nil {
				return nil, err
			}
			res[k] = r
		}
		return res, nil
	}
	return v, nil
}
