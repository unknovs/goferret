package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func contract(t *testing.T) *Contract {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "inputs.yaml")
	os.WriteFile(p, []byte(`version: 1
inputs:
  URL:    {type: url, required: true}
  KEY:    {type: string, secret: true, required: true}
  OPT:    {type: string}
  DEF:    {type: string, default: fallback}
  NUM:    {type: int}
`), 0o644)
	c, err := LoadContract(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func envOf(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestParseDotenv(t *testing.T) {
	in := string(rune(0xFEFF)) + "# comment\n\nA=1\r\nB=\"x # y \\\"q\\\" \\\\ \\n\"\nC= spaced  \nD=\n"
	got, err := ParseDotenv([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "1", "B": "x # y \"q\" \\ \n", "C": " spaced", "D": ""}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestParseDotenvErrors(t *testing.T) {
	for name, in := range map[string]string{
		"duplicate":    "A=1\nA=2\n",
		"no equals":    "A\n",
		"bad key":      "1A=x\n",
		"export":       "export A=1\n",
		"unterminated": "A=\"abc\n",
		"trailing":     "A=\"x\" y\n",
		"bad escape":   "A=\"\\t\"\n",
	} {
		_, err := ParseDotenv([]byte(in))
		if err == nil {
			t.Errorf("%s: expected an error", name)
			continue
		}
		if !strings.Contains(err.Error(), "line") {
			t.Errorf("%s: error should carry a line number: %v", name, err)
		}
	}
}

func TestDotenvErrorDoesNotLeakValue(t *testing.T) {
	_, err := ParseDotenv([]byte("SECRET=\"topsecretvalue\" trailing\n"))
	if err == nil || strings.Contains(err.Error(), "topsecretvalue") {
		t.Fatalf("error must exist and not contain the value: %v", err)
	}
}

func TestPrecedenceEnvOverDotenvOverDefault(t *testing.T) {
	c := contract(t)
	dot := map[string]string{"URL": "https://dot.example", "KEY": "dotkey123", "DEF": "fromdot"}
	vals, errs := c.Resolve([]string{"URL", "KEY", "DEF"}, Sources{Env: envOf(map[string]string{"URL": "https://env.example"}), Dotenv: dot})
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if vals["URL"].Value != "https://env.example" || vals["URL"].Source != "environment" {
		t.Errorf("env must win: %+v", vals["URL"])
	}
	if vals["KEY"].Value != "dotkey123" || !vals["KEY"].Secret {
		t.Errorf("dotenv value and secret flag: %+v", vals["KEY"])
	}
	if vals["DEF"].Value != "fromdot" {
		t.Errorf("dotenv beats default: %+v", vals["DEF"])
	}
	vals, _ = c.Resolve([]string{"DEF"}, Sources{})
	if vals["DEF"].Value != "fallback" || vals["DEF"].Source != "default" {
		t.Errorf("default: %+v", vals["DEF"])
	}
}

func TestEnvDoesNotMixWithLowerSourceFile(t *testing.T) {
	// env X must not be combined with a lower-priority .env X_FILE: env wins outright.
	c := contract(t)
	vals, errs := c.Resolve([]string{"KEY"}, Sources{
		Env:    envOf(map[string]string{"KEY": "envkey123"}),
		Dotenv: map[string]string{"KEY_FILE": "/does/not/exist"},
	})
	if len(errs) > 0 || vals["KEY"].Value != "envkey123" {
		t.Fatalf("%v %+v", errs, vals)
	}
}

func TestBothDirectAndFileIsError(t *testing.T) {
	c := contract(t)
	_, errs := c.Resolve([]string{"KEY"}, Sources{Env: envOf(map[string]string{"KEY": "", "KEY_FILE": "x"})})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "both KEY and KEY_FILE") {
		t.Fatalf("%v", errs)
	}
}

func TestFileValuePreservedExactly(t *testing.T) {
	c := contract(t)
	f := filepath.Join(t.TempDir(), "k.txt")
	os.WriteFile(f, []byte("  padded key\n"), 0o644)
	vals, errs := c.Resolve([]string{"KEY"}, Sources{Env: envOf(map[string]string{"KEY_FILE": f})})
	if len(errs) > 0 || vals["KEY"].Value != "  padded key\n" {
		t.Fatalf("no trimming allowed: %q %v", vals["KEY"].Value, errs)
	}
}

func TestMissingFileError(t *testing.T) {
	c := contract(t)
	_, errs := c.Resolve([]string{"KEY"}, Sources{Env: envOf(map[string]string{"KEY_FILE": filepath.Join(t.TempDir(), "nope")})})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "KEY_FILE") {
		t.Fatalf("%v", errs)
	}
}

func TestInvalidUTF8File(t *testing.T) {
	c := contract(t)
	f := filepath.Join(t.TempDir(), "bin")
	os.WriteFile(f, []byte{0xff, 0xfe, 0x00}, 0o644)
	_, errs := c.Resolve([]string{"KEY"}, Sources{Env: envOf(map[string]string{"KEY_FILE": f})})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "UTF-8") {
		t.Fatalf("%v", errs)
	}
}

func TestEmptyValueCountsAsPresent(t *testing.T) {
	c := contract(t)
	_, errs := c.Resolve([]string{"KEY"}, Sources{Env: envOf(map[string]string{"KEY": ""}), Dotenv: map[string]string{"KEY": "fallbackkey"}})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "empty") {
		t.Fatalf("an explicit empty env value must not fall back: %v", errs)
	}
	vals, errs := c.Resolve([]string{"OPT"}, Sources{Env: envOf(map[string]string{"OPT": ""})})
	if len(errs) > 0 || vals["OPT"].Value != "" {
		t.Fatalf("optional empty is allowed: %v %v", vals, errs)
	}
}

func TestTypeValidationAndMissingRequired(t *testing.T) {
	c := contract(t)
	_, errs := c.Resolve([]string{"URL", "NUM"}, Sources{Env: envOf(map[string]string{"URL": "not a url", "NUM": "x"})})
	if len(errs) != 2 {
		t.Fatalf("want 2 type errors: %v", errs)
	}
	_, errs = c.Resolve([]string{"URL"}, Sources{})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "required input URL") {
		t.Fatalf("%v", errs)
	}
}

func TestCheckDotenvKeys(t *testing.T) {
	c := contract(t)
	if err := c.CheckDotenvKeys(map[string]string{"URL": "x", "UNKNOWN": "y"}, "f"); err == nil || !strings.Contains(err.Error(), "UNKNOWN") {
		t.Fatalf("unknown key must be reported: %v", err)
	}
	if err := c.CheckDotenvKeys(map[string]string{"KEY": "x", "KEY_FILE": "y"}, "f"); err == nil {
		t.Fatal("X and X_FILE together must be reported")
	}
	if err := c.CheckDotenvKeys(map[string]string{"KEY_FILE": "y", "URL": "z"}, "f"); err != nil {
		t.Fatal(err)
	}
}

func TestContractRejectsReservedAndSecretDefault(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"reserved": "version: 1\ninputs:\n  X_FILE: {}\n",
		"secdef":   "version: 1\ninputs:\n  X: {secret: true, default: abc}\n",
		"unknown":  "version: 1\ninputs:\n  X: {nope: 1}\n",
		"version":  "version: 2\ninputs: {}\n",
	} {
		p := filepath.Join(dir, name+".yaml")
		os.WriteFile(p, []byte(body), 0o644)
		if _, err := LoadContract(p); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
