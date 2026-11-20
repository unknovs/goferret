// Package jsonptr implements RFC 6901 JSON Pointer evaluation over decoded JSON.
package jsonptr

import (
	"fmt"
	"strconv"
	"strings"
)

// Valid reports whether p is a syntactically valid pointer ("" is the root).
func Valid(p string) error {
	if p == "" {
		return nil
	}
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("JSON pointer %q must be empty or start with '/'", p)
	}
	for _, tok := range strings.Split(p[1:], "/") {
		for i := 0; i < len(tok); i++ {
			if tok[i] == '~' && (i+1 >= len(tok) || (tok[i+1] != '0' && tok[i+1] != '1')) {
				return fmt.Errorf("JSON pointer %q has an invalid '~' escape", p)
			}
		}
	}
	return nil
}

// Get evaluates p against doc (the result of encoding/json decoding into any).
// found is false when the location does not exist; err is only for an invalid
// pointer.
func Get(doc any, p string) (val any, found bool, err error) {
	if err := Valid(p); err != nil {
		return nil, false, err
	}
	if p == "" {
		return doc, true, nil
	}
	cur := doc
	for _, tok := range strings.Split(p[1:], "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		switch c := cur.(type) {
		case map[string]any:
			v, ok := c[tok]
			if !ok {
				return nil, false, nil
			}
			cur = v
		case []any:
			idx, perr := strconv.Atoi(tok)
			if perr != nil || idx < 0 || idx >= len(c) || (len(tok) > 1 && tok[0] == '0') {
				return nil, false, nil
			}
			cur = c[idx]
		default:
			return nil, false, nil
		}
	}
	return cur, true, nil
}
