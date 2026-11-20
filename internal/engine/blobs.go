package engine

import (
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Long base64 values (documents, certificates, signatures) make recorded
// bodies unreadable. They are replaced by a short marker; keys, structure and
// all short values stay exactly as they are:
//
//	"file":"UEsDBAAA..."        ->  "file":"<file: ZIP, 15023 bytes>"
//	"certificate":"MIIG..."     ->  "certificate":"<certificate: CN=..., serial ..., sha1 ..., sha256 ...>"
//
// Response bodies can additionally be saved as real files (--save-files).

const minBlobChars = 200

var blobRe = regexp.MustCompile(`(?:"([^"\\]{1,80})"\s*:\s*)?"([A-Za-z0-9+/]{200,}={0,2})"`)

// blobCtx says where saved files go. A zero value saves nothing.
type blobCtx struct {
	saveDir  string
	scenario string
	step     string
	counts   map[string]int
}

// shortenBlobs replaces long base64 strings in text. It returns the new text
// and the paths of any files it saved.
func shortenBlobs(text string, bc *blobCtx) (string, []string) {
	var saved []string
	var out strings.Builder
	last := 0
	for _, m := range blobRe.FindAllStringSubmatchIndex(text, -1) {
		raw, err := decodeBlob(text[m[4]:m[5]])
		if err != nil {
			continue
		}
		key := ""
		if m[2] >= 0 {
			key = text[m[2]:m[3]]
		}
		marker, kind := describeBlob(raw)
		if bc != nil && bc.saveDir != "" && kind != "" {
			if p, err := bc.save(raw, kind, key); err == nil {
				saved = append(saved, p)
				marker = strings.TrimSuffix(marker, ">") + ", saved to " + p + ">"
			}
		}
		out.WriteString(text[last:m[0]])
		if m[2] >= 0 {
			out.WriteString(text[m[0] : m[4]-1]) // the key and the colon, unchanged (without the blob's opening quote)
		}
		out.WriteString(`"` + marker + `"`)
		last = m[1]
	}
	if last == 0 {
		return text, nil
	}
	out.WriteString(text[last:])
	return out.String(), saved
}

func decodeBlob(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "="))
}

// describeBlob returns the marker text and, for documents worth saving, the
// file extension ("" for certificates and opaque binary data).
func describeBlob(raw []byte) (marker, saveExt string) {
	if cert, err := x509.ParseCertificate(raw); err == nil {
		s1 := sha1.Sum(raw)
		s256 := sha256.Sum256(raw)
		return fmt.Sprintf("<certificate: CN=%s, serial %s, sha1 %s, sha256 %s…>", cert.Subject.CommonName,
			strings.ToUpper(cert.SerialNumber.Text(16)), strings.ToUpper(hex.EncodeToString(s1[:])), strings.ToUpper(hex.EncodeToString(s256[:8]))), ""
	}
	n := len(raw)
	head := raw
	if len(head) > 200 {
		head = head[:200]
	}
	switch {
	case strings.HasPrefix(string(raw), "%PDF"):
		return fmt.Sprintf("<file: PDF, %d bytes>", n), "pdf"
	case n > 4 && raw[0] == 'P' && raw[1] == 'K':
		return fmt.Sprintf("<file: ZIP, %d bytes>", n), "zip"
	case len(raw) > 0 && (raw[0] == '{' || raw[0] == '[') && json.Valid(raw):
		return fmt.Sprintf("<file: JSON, %d bytes>", n), "json"
	case n > 1 && raw[0] == 0x30:
		return fmt.Sprintf("<DER data, %d bytes>", n), ""
	}
	return fmt.Sprintf("<binary data, %d bytes>", n), ""
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// save writes a returned document to <dir>/<scenario>/<step>.<key>.<ext>.
// An existing file with the same name is overwritten; nothing else in the
// directory is touched.
func (bc *blobCtx) save(raw []byte, ext, key string) (string, error) {
	if key == "" {
		key = "data"
	}
	if bc.counts == nil {
		bc.counts = map[string]int{}
	}
	dir := filepath.Join(bc.saveDir, unsafeName.ReplaceAllString(bc.scenario, "_"))
	base := unsafeName.ReplaceAllString(bc.step, "_") + "." + unsafeName.ReplaceAllString(key, "_")
	bc.counts[dir+"/"+base]++
	if n := bc.counts[dir+"/"+base]; n > 1 {
		base = fmt.Sprintf("%s-%d", base, n)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, base+"."+ext)
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		return "", err
	}
	return filepath.ToSlash(p), nil
}
