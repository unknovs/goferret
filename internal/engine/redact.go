package engine

import (
	"encoding/base64"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// MinSecretLen is the shortest secret value that is redacted by value.
// Shorter values would redact unrelated text; name-based rules still apply.
const MinSecretLen = 6

const redacted = "[REDACTED]"

// Redactor removes secrets from everything that is persisted or printed.
// Two rules apply: known secret values (and their base64 / URL-encoded forms)
// are replaced wherever they occur, and well-known sensitive header names and
// JSON field names are masked regardless of value.
type Redactor struct {
	values []string
}

var sensitiveHeaders = map[string]bool{
	"authorization": true, "proxy-authorization": true, "cookie": true, "set-cookie": true,
	"apikey": true, "api-key": true, "x-api-key": true,
}

var sensitiveField = regexp.MustCompile(`(?i)("(?:password|passwd|pwd|pfx|private[_-]?key|secret|client[_-]?secret|access[_-]?token|refresh[_-]?token|id[_-]?token|token|api[_-]?key|authorization)"\s*:\s*)"(?:[^"\\]|\\.)*"`)

// AddSecret registers a secret value and its common encodings.
func (r *Redactor) AddSecret(v string) {
	if len(v) < MinSecretLen {
		return
	}
	forms := []string{v, base64.StdEncoding.EncodeToString([]byte(v)), base64.URLEncoding.EncodeToString([]byte(v)), url.QueryEscape(v), url.PathEscape(v)}
	for _, f := range forms {
		if len(f) >= MinSecretLen && !contains(r.values, f) {
			r.values = append(r.values, f)
		}
	}
	// longest first so a long secret is replaced before any shorter overlap
	sort.Slice(r.values, func(i, j int) bool { return len(r.values[i]) > len(r.values[j]) })
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// String redacts a text value.
func (r *Redactor) String(s string) string {
	s = sensitiveField.ReplaceAllString(s, `$1"`+redacted+`"`)
	for _, v := range r.values {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, redacted)
		}
	}
	return s
}

// Headers redacts a header map (names are compared case-insensitively).
func (r *Redactor) Headers(h map[string]string) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		if sensitiveHeaders[strings.ToLower(k)] {
			out[k] = redacted
		} else {
			out[k] = r.String(v)
		}
	}
	return out
}
