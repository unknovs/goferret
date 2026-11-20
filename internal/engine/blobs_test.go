package engine

import (
	"context"
	"strconv"

	"archive/zip"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"github.com/unknovs/goferret/internal/config"
	"github.com/unknovs/goferret/internal/results"
	"github.com/unknovs/goferret/internal/suite"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func makeCert(t *testing.T) []byte {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(0x1A2B3C4D), Subject: pkix.Name{CommonName: "Test Subject ü"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func makeZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.CreateHeader(&zip.FileHeader{Name: "mimetype", Method: zip.Store})
	w.Write([]byte("application/zip"))
	w2, _ := zw.Create("data/records.xml")
	w2.Write(bytes.Repeat([]byte("<x/>"), 500))
	zw.Close()
	return buf.Bytes()
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func TestCertificatesBecomeSerialAndThumbprints(t *testing.T) {
	der := makeCert(t)
	s1, s256 := sha1.Sum(der), sha256.Sum256(der)
	in := `{"certificate":"` + b64(der) + `","id":"abc","n":1}`
	out, saved := shortenBlobs(in, nil)
	if len(saved) != 0 {
		t.Fatal("nothing is saved without a save directory")
	}
	for _, want := range []string{
		`"certificate":"<certificate: CN=Test Subject ü, serial 1A2B3C4D, sha1 ` + strings.ToUpper(hex.EncodeToString(s1[:])),
		`sha256 ` + strings.ToUpper(hex.EncodeToString(s256[:8])) + `…>"`,
		`"id":"abc","n":1}`, // everything else is untouched
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, b64(der)[:40]) {
		t.Error("the base64 must be gone")
	}
}

func TestFilesBecomeMarkersWithTypeAndSizeAndStructureIsKept(t *testing.T) {
	zipData := makeZip(t)
	pdf := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte{'x'}, 3000)...)
	in := `{"documents":[{"file":"` + b64(zipData) + `","fileName":"a b.zip","id":"doc-1"}],"preview":"` + b64(pdf) + `","lang":"en"}`
	out, _ := shortenBlobs(in, nil)
	want := `{"documents":[{"file":"<file: ZIP, ` + itoa(len(zipData)) + ` bytes>","fileName":"a b.zip","id":"doc-1"}],"preview":"<file: PDF, ` + itoa(len(pdf)) + ` bytes>","lang":"en"}`
	if out != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
	// short values, ordinary words and invalid base64 are left alone
	same := `{"a":"` + strings.Repeat("hello world! ", 40) + `","b":"short","c":"` + strings.Repeat("AB", 50) + `"}`
	if got, _ := shortenBlobs(same, nil); got != same {
		t.Errorf("text that is not a long base64 value must not change:\n%s", got)
	}
}

func TestOtherBlobKindsAndBrokenCertificates(t *testing.T) {
	cases := map[string][]byte{
		"<DER data, 400 bytes>":    append([]byte{0x30, 0x82, 0x01, 0x8c}, bytes.Repeat([]byte{1}, 396)...), // not a certificate
		"<file: JSON, ":            []byte(`{"payload":"` + strings.Repeat("x", 300) + `"}`),
		"<binary data, 300 bytes>": bytes.Repeat([]byte{0xff, 0x00, 0x7f}, 100),
		"<file: ZIP, ":             append([]byte("PK\x03\x04"), bytes.Repeat([]byte{2}, 400)...),
	}
	for want, raw := range cases {
		out, _ := shortenBlobs(`{"v":"`+b64(raw)+`"}`, nil)
		if !strings.Contains(out, want) {
			t.Errorf("%q not in %s", want, out)
		}
	}
}

func TestSaveFilesWritesRealDocumentsAndNeverCertificates(t *testing.T) {
	zipData, cert := makeZip(t), makeCert(t)
	dir := t.TempDir()
	bc := &blobCtx{saveDir: dir, scenario: "docs.fetch", step: "fetch"}
	in := `{"results":[{"id":"x","file":"` + b64(zipData) + `"},{"id":"y","file":"` + b64(zipData) + `"}],"clientCertificate":"` + b64(cert) + `"}`
	out, saved := shortenBlobs(in, bc)
	if len(saved) != 2 {
		t.Fatalf("two documents expected, saved: %v", saved)
	}
	want1 := filepath.ToSlash(filepath.Join(dir, "docs.fetch", "fetch.file.zip"))
	want2 := filepath.ToSlash(filepath.Join(dir, "docs.fetch", "fetch.file-2.zip"))
	if saved[0] != want1 || saved[1] != want2 {
		t.Fatalf("names carry scenario, step and field, repeats are numbered:\n%v", saved)
	}
	got, err := os.ReadFile(saved[0])
	if err != nil || !bytes.Equal(got, zipData) {
		t.Fatalf("the saved file must be the exact document bytes: %v", err)
	}
	if !strings.Contains(out, "saved to "+want1+">") || !strings.Contains(out, "<certificate: ") {
		t.Errorf("the marker says where the file went; certificates are described, not saved:\n%s", out)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "docs.fetch"))
	if len(entries) != 2 {
		t.Errorf("only documents are written, not certificates: %v", entries)
	}
	// nothing is saved when no directory was asked for
	if _, s := shortenBlobs(in, nil); len(s) != 0 {
		t.Error("no save directory, no files")
	}
}

func TestSaveFilesEndToEndAndTheLogShowsOnlyTheMarker(t *testing.T) {
	zipData := makeZip(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"results":[{"id":"s1","file":"` + b64(zipData) + `"}]}`))
	}))
	defer srv.Close()
	s := load(t, header, `version: 1
scenarios:
  - id: docs.one
    steps:
      - id: fetch
        service: api
        method: POST
        path: /f
        json: { id: s1 }
        assert: [ { name: ok, checks: [ { status: 200 } ] } ]
`)
	saveDir := t.TempDir()
	res, out := runWith(t, s, inputs(srv.URL), Selection{}, Options{SaveDir: saveDir})
	if !res.AllPassed() {
		t.Fatal("run must pass")
	}
	p := filepath.Join(saveDir, "docs.one", "fetch.file.zip")
	got, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(got, zipData) {
		t.Fatalf("returned document must be saved as a real file: %v", err)
	}
	call := res.Calls[0]
	if len(call.SavedFiles) != 1 || !strings.Contains(call.ResBody.Preview, "<file: ZIP,") || strings.Contains(call.ResBody.Preview, b64(zipData)[:60]) {
		t.Fatalf("results hold the marker, not the base64: %+v", call)
	}
	if _, err := os.Stat(filepath.Join(out, "attachments")); err == nil {
		t.Error("no attachments folder")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// runWith is run with extra Options (for example SaveDir).
func runWith(t *testing.T, s *suite.Suite, in map[string]config.Value, sel Selection, opt Options) (*results.Results, string) {
	t.Helper()
	out := t.TempDir()
	opt.OutDir = out
	chosen, err := Select(s, sel)
	if err != nil {
		t.Fatal(err)
	}
	res := New(s, in, opt).Run(context.Background(), chosen, sel)
	if err := results.WriteAll(out, res); err != nil {
		t.Fatal(err)
	}
	return res, out
}
