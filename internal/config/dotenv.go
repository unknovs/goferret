// Package config resolves suite inputs from the process environment, an
// explicit dotenv file and contract defaults.
package config

import (
	"fmt"
	"os"
	"strings"
)

// ParseDotenv parses the strict dotenv subset described in the README:
// KEY=value lines, '#' comment lines, unquoted values taken literally,
// double-quoted values with \" \\ \n escapes. No expansion, no multi-line
// values, no export prefix. Duplicate keys and malformed lines are errors.
// Errors carry the line number but never the value.
func ParseDotenv(data []byte) (map[string]string, error) {
	out := map[string]string{}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.TrimPrefix(text, string(rune(0xFEFF))) // UTF-8 byte order mark
	for i, line := range strings.Split(text, "\n") {
		n := i + 1
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("line %d: expected KEY=value", n)
		}
		key := strings.TrimSpace(line[:eq])
		if !validKey(key) {
			return nil, fmt.Errorf("line %d: invalid key %q (use letters, digits and underscore, not starting with a digit; 'export' is not supported)", n, key)
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("line %d: duplicate key %s", n, key)
		}
		raw := line[eq+1:]
		val, err := parseValue(raw)
		if err != nil {
			return nil, fmt.Errorf("line %d: %s: %v", n, key, err)
		}
		out[key] = val
	}
	return out, nil
}

// ReadDotenv reads and parses an explicitly selected dotenv file.
func ReadDotenv(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("env file %s: %v", path, err)
	}
	m, err := ParseDotenv(data)
	if err != nil {
		return nil, fmt.Errorf("env file %s: %v", path, err)
	}
	return m, nil
}

func validKey(k string) bool {
	if k == "" || (k[0] >= '0' && k[0] <= '9') {
		return false
	}
	for _, c := range k {
		if c != '_' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func parseValue(raw string) (string, error) {
	if !strings.HasPrefix(raw, `"`) {
		return strings.TrimRight(raw, " \t"), nil // unquoted: literal, no inline comments
	}
	var b strings.Builder
	rest := raw[1:]
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		switch c {
		case '\\':
			i++
			if i >= len(rest) {
				return "", fmt.Errorf("unterminated escape")
			}
			switch rest[i] {
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case 'n':
				b.WriteByte('\n')
			default:
				return "", fmt.Errorf("unsupported escape \\%c", rest[i])
			}
		case '"':
			if strings.TrimSpace(rest[i+1:]) != "" {
				return "", fmt.Errorf("unexpected text after closing quote")
			}
			return b.String(), nil
		default:
			b.WriteByte(c)
		}
	}
	return "", fmt.Errorf("unterminated quoted value")
}
