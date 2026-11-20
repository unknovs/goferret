package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/unknovs/goferret/internal/results"
	"github.com/unknovs/goferret/internal/suite"
	"github.com/unknovs/goferret/internal/tmpl"
)

// Limits for recorded bodies. See the "Results" section of the README.
const (
	PreviewBytes    = 64 * 1024 // recorded body text is cut here (after secrets and long base64 are shortened)
	MaxResponseBody = 128 * 1024 * 1024
)

func newClient() *http.Client {
	return &http.Client{
		// Timeouts are applied per request through the context (see do), so a
		// step can override the suite default.
		// Redirects are not followed: the 3xx response is the result, so a
		// mutating request is never silently replayed.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// builtRequest is a rendered step.
type builtRequest struct {
	req  *http.Request
	body []byte
	note string // non-empty when the body is not recorded (binary upload)
}

// buildRequest renders a step into an HTTP request.
func (r *Runner) buildRequest(ctx context.Context, st suite.Step, sc *Scope) (*builtRequest, error) {
	svc, ok := r.Suite.Services[st.Service]
	if !ok {
		return nil, fmt.Errorf("unknown service %q", st.Service)
	}
	base, err := r.R.Str(svc.BaseURL, sc)
	if err != nil {
		return nil, fmt.Errorf("service %s baseUrl: %v", st.Service, err)
	}
	path, err := r.R.Str(st.Path, sc)
	if err != nil {
		return nil, fmt.Errorf("path: %v", err)
	}
	u := strings.TrimRight(base, "/") + path
	if len(st.Query) > 0 {
		keys := make([]string, 0, len(st.Query))
		for k := range st.Query {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		q := url.Values{}
		for _, k := range keys {
			v, err := r.R.Str(st.Query[k], sc)
			if err != nil {
				return nil, fmt.Errorf("query %s: %v", k, err)
			}
			q.Set(k, v)
		}
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		u += sep + q.Encode()
	}

	var body []byte
	ctype, note := "", ""
	switch {
	case st.JSON != nil:
		v, err := r.R.Any(st.JSON, sc)
		if err != nil {
			return nil, fmt.Errorf("json body: %v", err)
		}
		if body, err = json.Marshal(v); err != nil {
			return nil, fmt.Errorf("json body: %v", err)
		}
		ctype = "application/json"
	case st.Body != "":
		s, err := r.R.Str(st.Body, sc)
		if err != nil {
			return nil, fmt.Errorf("body: %v", err)
		}
		body = []byte(s)
		ctype = "text/plain; charset=utf-8"
	case len(st.Form) > 0:
		form := url.Values{}
		for k, v := range st.Form {
			s, err := r.R.Str(v, sc)
			if err != nil {
				return nil, fmt.Errorf("form %s: %v", k, err)
			}
			form.Set(k, s)
		}
		body = []byte(form.Encode())
		ctype = "application/x-www-form-urlencoded"
	case st.Multipart != nil:
		b, ct, files, err := r.buildMultipart(st.Multipart, sc)
		if err != nil {
			return nil, err
		}
		body, ctype = b, ct
		note = fmt.Sprintf("multipart upload of %d file(s), %d bytes (file content is not recorded)", files, len(body))
	}

	req, err := http.NewRequestWithContext(ctx, st.Method, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if !st.NoService {
		for k, v := range svc.Headers {
			s, err := r.R.Str(v, sc)
			if err != nil {
				return nil, fmt.Errorf("service %s header %s: %v", st.Service, k, err)
			}
			req.Header.Set(k, s)
		}
		for k, v := range svc.OptionalHeaders {
			s, err := r.R.Str(v, sc)
			if err != nil {
				if isUndefined(v, sc) {
					continue // optional input not set: omit the header
				}
				return nil, fmt.Errorf("service %s header %s: %v", st.Service, k, err)
			}
			req.Header.Set(k, s)
		}
	}
	for k, v := range st.Headers {
		s, err := r.R.Str(v, sc)
		if err != nil {
			return nil, fmt.Errorf("header %s: %v", k, err)
		}
		req.Header.Set(k, s)
	}
	return &builtRequest{req: req, body: body, note: note}, nil
}

func (r *Runner) buildMultipart(m *suite.Multipart, sc *Scope) ([]byte, string, int, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fields := make([]string, 0, len(m.Fields))
	for k := range m.Fields {
		fields = append(fields, k)
	}
	sort.Strings(fields)
	for _, k := range fields {
		s, err := r.R.Str(m.Fields[k], sc)
		if err != nil {
			return nil, "", 0, fmt.Errorf("multipart field %s: %v", k, err)
		}
		_ = mw.WriteField(k, s)
	}
	files := make([]string, 0, len(m.Files))
	for k := range m.Files {
		files = append(files, k)
	}
	sort.Strings(files)
	for _, k := range files {
		rel, err := r.R.Str(m.Files[k], sc)
		if err != nil {
			return nil, "", 0, fmt.Errorf("multipart file %s: %v", k, err)
		}
		data, err := r.R.readFixture(rel)
		if err != nil {
			return nil, "", 0, err
		}
		part, err := mw.CreateFormFile(k, filepath.Base(rel))
		if err != nil {
			return nil, "", 0, err
		}
		_, _ = part.Write(data)
	}
	if err := mw.Close(); err != nil {
		return nil, "", 0, err
	}
	return buf.Bytes(), mw.FormDataContentType(), len(files), nil
}

// isUndefined reports whether a variable referenced in v is not defined.
func isUndefined(v string, sc *Scope) bool {
	parts, perr := tmpl.Parse(v)
	if perr != nil {
		return false
	}
	for _, p := range parts {
		if p.Var != "" {
			if _, ok := sc.Get(p.Var); !ok {
				return true
			}
		}
	}
	return false
}

// do performs one request and records it. The returned Response is nil when
// no response was received (transport error).
func (r *Runner) do(ctx context.Context, scenario string, st suite.Step, sc *Scope, attempt int) (*Response, error) {
	timeout := r.Timeout
	if st.TimeoutSeconds > 0 {
		timeout = time.Duration(st.TimeoutSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	br, err := r.buildRequest(ctx, st, sc)
	if err != nil {
		return nil, err
	}
	req := br.req
	r.seq++
	call := results.Call{
		Seq: r.seq, Scenario: scenario, Step: st.ID, Service: st.Service, Attempt: attempt,
		Method: st.Method, URL: r.Redact.String(req.URL.String()),
		ReqHeaders: r.Redact.Headers(flatten(req.Header)),
	}
	if br.note != "" {
		call.ReqBody = results.Body{Bytes: len(br.body), Omitted: br.note}
	} else {
		call.ReqBody, _ = r.recordBody(br.body, nil)
	}
	start := time.Now()
	res, err := r.Client.Do(req)
	if err != nil {
		call.DurationMs = time.Since(start).Milliseconds()
		call.Error = r.Redact.String(unwrapURLError(err))
		r.calls = append(r.calls, call)
		r.traceCall(call)
		return nil, fmt.Errorf("request failed: %s", call.Error)
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, MaxResponseBody+1))
	elapsed := time.Since(start)
	call.DurationMs = elapsed.Milliseconds()
	if err == nil && len(data) > MaxResponseBody {
		err = fmt.Errorf("response body exceeds %d bytes", MaxResponseBody)
	}
	call.Status = res.StatusCode
	call.ResHeaders = r.Redact.Headers(flatten(res.Header))
	call.ResBody, call.SavedFiles = r.recordBody(data, &blobCtx{saveDir: r.SaveDir, scenario: scenario, step: st.ID})
	if err != nil {
		call.Error = r.Redact.String(err.Error())
		r.calls = append(r.calls, call)
		r.traceCall(call)
		return nil, fmt.Errorf("reading response: %s", call.Error)
	}
	r.calls = append(r.calls, call)
	r.traceCall(call)
	return &Response{Status: res.StatusCode, Header: res.Header, Body: data, Duration: elapsed}, nil
}

func unwrapURLError(err error) string {
	if ue, ok := err.(*url.Error); ok {
		return ue.Op + " " + ue.Err.Error()
	}
	return err.Error()
}

func flatten(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = strings.Join(v, ", ")
	}
	return out
}

// recordBody sanitizes a body for the results and the log: secrets are
// redacted, long base64 values become short markers (see blobs.go), and the
// text is cut at PreviewBytes. Bodies that are not valid UTF-8 text are
// omitted with a reason instead of being written raw. It returns the paths of
// any documents saved because of --save-files.
func (r *Runner) recordBody(b []byte, bc *blobCtx) (results.Body, []string) {
	rec := results.Body{Bytes: len(b)}
	if len(b) == 0 {
		return rec, nil
	}
	if !utf8.Valid(b) {
		rec.Omitted = "binary body not recorded (cannot be sanitized safely)"
		return rec, nil
	}
	text := r.Redact.String(string(b))
	text, saved := shortenBlobs(text, bc)
	if len(text) <= PreviewBytes {
		rec.Preview = text
		return rec, saved
	}
	rec.Preview = strings.ToValidUTF8(text[:PreviewBytes], "") + "…"
	rec.Truncated = true
	return rec, saved
}
