package config

import (
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// InputDef declares one input a suite accepts.
type InputDef struct {
	Type        string  `yaml:"type"` // string (default), url, int
	Required    bool    `yaml:"required"`
	Secret      bool    `yaml:"secret"`
	Description string  `yaml:"description"`
	Default     *string `yaml:"default"`
}

// Contract is the parsed inputs.yaml.
type Contract struct {
	Version int                 `yaml:"version"`
	Inputs  map[string]InputDef `yaml:"inputs"`
}

// Value is a resolved input.
type Value struct {
	Value  string
	Secret bool
	Source string // "env", "env file", "file <path>", "default"
}

// LoadContract reads and validates an inputs.yaml file.
func LoadContract(path string) (*Contract, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Contract
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	if c.Version != 1 {
		return nil, fmt.Errorf("%s: unsupported version %d (want 1)", path, c.Version)
	}
	for name, d := range c.Inputs {
		if !validKey(name) || strings.HasSuffix(name, "_FILE") {
			return nil, fmt.Errorf("%s: invalid input name %q (the _FILE suffix is reserved)", path, name)
		}
		switch d.Type {
		case "", "string", "url", "int":
		default:
			return nil, fmt.Errorf("%s: input %s: unknown type %q", path, name, d.Type)
		}
		if d.Secret && d.Default != nil {
			return nil, fmt.Errorf("%s: input %s: a secret input cannot have a default", path, name)
		}
	}
	return &c, nil
}

// Sources are the places inputs come from.
type Sources struct {
	Env     func(string) (string, bool) // process environment lookup
	Dotenv  map[string]string           // explicit --env-file content, may be nil
	DotPath string
}

// CheckDotenvKeys rejects keys in the dotenv file that are not declared
// inputs or their _FILE variants, and X together with X_FILE.
func (c *Contract) CheckDotenvKeys(dot map[string]string, path string) error {
	var errs []string
	for k := range dot {
		base := strings.TrimSuffix(k, "_FILE")
		if _, ok := c.Inputs[base]; !ok {
			errs = append(errs, fmt.Sprintf("%s: unknown key %s (not a declared input)", path, k))
			continue
		}
		if strings.HasSuffix(k, "_FILE") {
			if _, both := dot[base]; both {
				errs = append(errs, fmt.Sprintf("%s: both %s and %s are set", path, base, k))
			}
		}
	}
	sort.Strings(errs)
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "\n"))
	}
	return nil
}

// Resolve resolves the named inputs. Precedence, highest first:
// env X, env X_FILE, .env X, .env X_FILE, contract default. Setting X and
// X_FILE in the same source is an error. An explicitly empty value counts as
// present. File contents are used exactly as read (no trimming).
// Errors name the input and source, never the value.
func (c *Contract) Resolve(names []string, src Sources) (map[string]Value, []error) {
	out := map[string]Value{}
	var errs []error
	for _, name := range names {
		def, ok := c.Inputs[name]
		if !ok {
			errs = append(errs, fmt.Errorf("input %s is not declared in the inputs contract", name))
			continue
		}
		val, found, err := c.resolveOne(name, def, src)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !found {
			if def.Required {
				errs = append(errs, fmt.Errorf("required input %s is not set (set %s or %s_FILE in the environment or the env file)", name, name, name))
			}
			continue
		}
		if err := validateType(name, def, val.Value); err != nil {
			errs = append(errs, err)
			continue
		}
		val.Secret = def.Secret
		out[name] = val
	}
	return out, errs
}

func (c *Contract) resolveOne(name string, def InputDef, src Sources) (Value, bool, error) {
	type lookup struct {
		get    func(string) (string, bool)
		label  string
		isFile bool
	}
	envGet := src.Env
	if envGet == nil {
		envGet = func(string) (string, bool) { return "", false }
	}
	dotGet := func(k string) (string, bool) { v, ok := src.Dotenv[k]; return v, ok }
	for _, s := range []struct {
		get   func(string) (string, bool)
		label string
	}{{envGet, "environment"}, {dotGet, "env file"}} {
		direct, hasDirect := s.get(name)
		file, hasFile := s.get(name + "_FILE")
		if hasDirect && hasFile {
			return Value{}, false, fmt.Errorf("input %s: both %s and %s_FILE are set in the %s", name, name, name, s.label)
		}
		if hasDirect {
			if !utf8.ValidString(direct) {
				return Value{}, false, fmt.Errorf("input %s (%s): value is not valid UTF-8", name, s.label)
			}
			return Value{Value: direct, Source: s.label}, true, nil
		}
		if hasFile {
			if file == "" {
				return Value{}, false, fmt.Errorf("input %s: %s_FILE in the %s is empty", name, name, s.label)
			}
			data, err := os.ReadFile(file)
			if err != nil {
				return Value{}, false, fmt.Errorf("input %s: cannot read %s_FILE (%s) from the %s: %v", name, name, file, s.label, errNoContent(err))
			}
			if !utf8.Valid(data) {
				return Value{}, false, fmt.Errorf("input %s: file %s is not valid UTF-8 text", name, file)
			}
			return Value{Value: string(data), Source: "file " + file}, true, nil
		}
	}
	if def.Default != nil {
		return Value{Value: *def.Default, Source: "default"}, true, nil
	}
	return Value{}, false, nil
}

func errNoContent(err error) error {
	if pe, ok := err.(*os.PathError); ok {
		return pe.Err
	}
	return err
}

func validateType(name string, def InputDef, v string) error {
	if def.Required && v == "" {
		return fmt.Errorf("input %s: required but empty", name)
	}
	switch def.Type {
	case "url":
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("input %s: not a valid http(s) URL", name)
		}
	case "int":
		if _, err := strconv.Atoi(v); err != nil {
			return fmt.Errorf("input %s: not an integer", name)
		}
	}
	return nil
}
