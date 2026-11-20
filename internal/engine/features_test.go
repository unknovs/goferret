package engine

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unknovs/goferret/internal/results"
)

func TestSetFormMultipartAndDecodeJSON(t *testing.T) {
	var formBody, mpFile, mpField, mpName atomic.Value
	payload := base64.StdEncoding.EncodeToString([]byte(`{"payload":"ENVELOPING","n":2}`))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			b, _ := io.ReadAll(r.Body)
			formBody.Store(r.Header.Get("Content-Type") + "|" + string(b))
			w.Write([]byte(`{"access_token":"tok-secret-998877"}`))
		case "/upload":
			mt, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if mt == "multipart/form-data" {
				mr := multipart.NewReader(r.Body, params["boundary"])
				for {
					p, err := mr.NextPart()
					if err != nil {
						break
					}
					data, _ := io.ReadAll(p)
					if p.FileName() != "" {
						mpFile.Store(string(data))
						mpName.Store(p.FormName() + ":" + p.FileName())
					} else {
						mpField.Store(p.FormName() + "=" + string(data))
					}
				}
			}
			w.Write([]byte(`{"fileJson":"` + payload + `"}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	s := load(t, header, `version: 1
scenarios:
  - id: feat
    steps:
      - id: token
        service: api
        method: POST
        path: /token
        set: { sid: "sess-{{$random_hex 6}}", scope: "read write" }
        form: { grant_type: client_credentials, scope: "{{scope}}", sid: "{{sid}}" }
        capture: { tok: { json: /access_token, secret: true } }
        assert: [ { name: got token, checks: [ { status: 200 } ] } ]
      - id: up
        service: api
        method: PUT
        path: /upload
        headers: { Authorization: "Bearer {{tok}}" }
        multipart: { files: { file: f.txt }, fields: { note: "for {{sid}}" } }
        assert:
          - name: decoded payload
            checks:
              - { json: /fileJson, decodeJson: /payload, exists: true }
              - { json: /fileJson, decodeJson: /payload, eq: ENVELOPING }
              - { json: /fileJson, decodeJson: /n, type: number }
          - name: decoded absent
            checks:
              - { json: /fileJson, decodeJson: /nope, absent: true }
          - name: fast enough
            checks:
              - { responseTimeLtMs: 5000 }
`)
	res, out := run(t, s, inputs(srv.URL), Selection{})
	if !res.AllPassed() {
		b, _ := json.MarshalIndent(res.Scenarios, "", " ")
		t.Fatalf("%s", b)
	}
	fb := formBody.Load().(string)
	if !strings.HasPrefix(fb, "application/x-www-form-urlencoded|") || !strings.Contains(fb, "grant_type=client_credentials") || !strings.Contains(fb, "scope=read+write") || !strings.Contains(fb, "sid=sess-") {
		t.Errorf("form body: %s", fb)
	}
	if mpFile.Load() != "hello" || mpName.Load() != "file:f.txt" || !strings.HasPrefix(mpField.Load().(string), "note=for sess-") {
		t.Errorf("multipart: %v %v %v", mpFile.Load(), mpName.Load(), mpField.Load())
	}
	up := res.Calls[1]
	if up.ReqBody.Omitted == "" || up.ReqBody.Preview != "" {
		t.Errorf("a multipart upload must not record file content: %+v", up.ReqBody)
	}
	for _, c := range res.Calls {
		if strings.Contains(c.ReqHeaders["Authorization"], "tok-secret-998877") {
			t.Error("Authorization header leaked")
		}
	}
	_ = out
}

func TestDecodeJSONErrorsAreExecutionErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"a":"not base64 !!","b":"` + base64.StdEncoding.EncodeToString([]byte("plain text")) + `"}`))
	}))
	defer srv.Close()
	s := load(t, header, `version: 1
scenarios:
  - id: d
    steps:
      - id: s
        service: api
        method: GET
        path: /x
        assert:
          - { name: bad base64, checks: [ { json: /a, decodeJson: /p, exists: true } ] }
          - { name: not json, checks: [ { json: /b, decodeJson: /p, exists: true } ] }
`)
	res, _ := run(t, s, inputs(srv.URL), Selection{})
	g := scenario(res, "d").Steps[0].Groups
	if g[0].Status != results.Errored || g[1].Status != results.Errored {
		t.Fatalf("%+v", g)
	}
}

func TestPerStepTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	s := load(t, header, `version: 1
scenarios:
  - id: slow
    steps:
      - id: short
        service: api
        method: GET
        path: /x
        timeoutSeconds: 1
        assert: [ { name: ok, checks: [ { status: 200 } ] } ]
      - id: after
        service: api
        method: GET
        path: /x
`)
	res, _ := run(t, s, inputs(srv.URL), Selection{})
	sc := scenario(res, "slow")
	if sc.Steps[0].Status != results.Errored || sc.Steps[1].Status != results.Blocked || res.Calls[0].Error == "" {
		t.Fatalf("a step timeout is an error that blocks the rest: %+v", sc.Steps)
	}
}

func TestUTCNowMinusHoursBuiltin(t *testing.T) {
	rd := &Renderer{}
	got, err := rd.Str("{{$utc_now_minus_hours 24}}", NewScope(nil))
	if err != nil {
		t.Fatal(err)
	}
	ts, err := time.Parse("2006-01-02T15:04:05Z", got)
	if err != nil {
		t.Fatalf("format: %q: %v", got, err)
	}
	if d := time.Since(ts); d < 23*time.Hour+59*time.Minute || d > 24*time.Hour+time.Minute {
		t.Fatalf("expected about 24h ago, got %v ago", d)
	}
	if _, err := rd.Str("{{$utc_now_minus_hours -1}}", NewScope(nil)); err == nil {
		t.Error("negative hours must be rejected")
	}
}

func TestBase64OfDerivedSecretIsRedacted(t *testing.T) {
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Header.Get("X-Basic"))
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	s := load(t, header, `version: 1
scenarios:
  - id: basic
    steps:
      - id: s
        service: api
        method: GET
        path: /x
        set: { creds: "client:{{KEY}}" }
        headers: { X-Basic: "Basic {{$base64_of creds}}" }
        assert: [ { name: ok, checks: [ { status: 200 } ] } ]
`)
	res, out := run(t, s, inputs(srv.URL), Selection{})
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("client:s3cr3t-key-123"))
	if !res.AllPassed() || got.Load() != want {
		t.Fatalf("header %v, want %s", got.Load(), want)
	}
	enc := base64.StdEncoding.EncodeToString([]byte("client:s3cr3t-key-123"))
	for _, f := range []string{"results.json", "junit.xml"} {
		data, _ := os.ReadFile(filepath.Join(out, f))
		if strings.Contains(string(data), enc) || strings.Contains(string(data), "s3cr3t-key-123") {
			t.Errorf("%s leaks the derived credential", f)
		}
	}
}

func TestCleanupForAResourceThatWasNeverCreatedIsSkippedNotErrored(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	s := load(t, header, `version: 1
scenarios:
  - id: cleanup
    steps:
      - id: create
        service: api
        method: POST
        path: /create
        capture: { session: { json: /id } }
        assert: [ { name: created, checks: [ { status: 200 } ] } ]
    cleanup:
      - { id: close, service: api, method: DELETE, path: "/session/{{session}}" }
`)
	res, _ := run(t, s, inputs(srv.URL), Selection{})
	sc := scenario(res, "cleanup")
	if sc.Status != results.Failed && sc.Status != results.Errored {
		t.Fatalf("the scenario must still fail: %s", sc.Status)
	}
	last := sc.Steps[len(sc.Steps)-1]
	if last.ID != "close" || last.Status != results.Blocked || !strings.Contains(last.Reason, "cleanup skipped") {
		t.Fatalf("cleanup must be skipped, not errored: %+v", last)
	}
	if len(res.Calls) != 1 {
		t.Fatalf("no request may be sent for the skipped cleanup: %d calls", len(res.Calls))
	}
}
