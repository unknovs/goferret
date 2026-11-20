// Package report renders a saved results.json into one self-contained HTML
// file. It needs no credentials, no suite definitions and no network: the
// results are embedded in the page and rendered by inline code.
package report

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/unknovs/goferret/internal/results"
)

//go:embed template.html
var pageTemplate string

// Result describes what Build did, so the CLI can say it explicitly.
type Result struct {
	Output      string
	Copied      int      // attachments copied next to the report
	Missing     []string // referenced attachments that do not exist
	Attachments int      // distinct attachments referenced
}

// Build validates inputPath, embeds its content in the shared template and
// writes outputPath. Attachments referenced by the results are resolved
// relative to the input file; when the report is written to another
// directory they are copied so the relative links keep working.
func Build(inputPath, outputPath string) (*Result, error) {
	r, err := results.Load(inputPath)
	if err != nil {
		return nil, err
	}
	if err := checkConsistent(r); err != nil {
		return nil, fmt.Errorf("%s: %v", inputPath, err)
	}
	atts, err := attachments(r)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", inputPath, err)
	}

	inDir, _ := filepath.Abs(filepath.Dir(inputPath))
	outAbs, _ := filepath.Abs(outputPath)
	outDir := filepath.Dir(outAbs)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	res := &Result{Output: outputPath, Attachments: len(atts)}
	for _, rel := range atts {
		src := filepath.Join(inDir, filepath.FromSlash(rel))
		if _, err := os.Stat(src); err != nil {
			res.Missing = append(res.Missing, rel)
			continue
		}
		if outDir == inDir {
			continue
		}
		dst := filepath.Join(outDir, filepath.FromSlash(rel))
		if err := copyFile(src, dst); err != nil {
			return nil, fmt.Errorf("copy attachment %s: %v", rel, err)
		}
		res.Copied++
	}

	page, err := render(r, res.Missing)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(outAbs, page, 0o644); err != nil {
		return nil, fmt.Errorf("write %s: %v", outputPath, err)
	}
	return res, nil
}

// checkConsistent rejects files whose stored totals do not match their records.
func checkConsistent(r *results.Results) error {
	stored := r.Totals
	r.Compute()
	if stored != r.Totals {
		return fmt.Errorf("inconsistent results: stored totals %+v do not match the recorded scenarios %+v", stored, r.Totals)
	}
	for _, c := range r.Calls {
		if c.Seq <= 0 {
			return fmt.Errorf("inconsistent results: a call has no sequence number")
		}
	}
	return nil
}

// attachments returns the distinct attachment paths, rejecting unsafe ones.
func attachments(r *results.Results) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, c := range r.Calls {
		for _, a := range []string{c.ReqBody.Attachment, c.ResBody.Attachment} {
			if a == "" || seen[a] {
				continue
			}
			if err := safeRel(a); err != nil {
				return nil, err
			}
			seen[a] = true
			out = append(out, a)
		}
	}
	return out, nil
}

func safeRel(p string) error {
	clean := path.Clean(p)
	if strings.Contains(p, "\\") || path.IsAbs(p) || clean != p || strings.HasPrefix(clean, "..") || !strings.HasPrefix(clean, "attachments/") || strings.Contains(p, ":") {
		return fmt.Errorf("unsafe attachment path %q (attachments must be relative paths under attachments/)", p)
	}
	return nil
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

type pageData struct {
	Title   string
	Data    template.JS
	Missing template.JS
}

// render embeds the results as JSON. The JSON encoder escapes <, > and &
// (and U+2028/9), so no payload can close the script element or inject markup.
func render(r *results.Results, missing []string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(true)
	if err := enc.Encode(r); err != nil {
		return nil, err
	}
	data := bytes.TrimSpace(buf.Bytes())
	var mbuf bytes.Buffer
	menc := json.NewEncoder(&mbuf)
	menc.SetEscapeHTML(true)
	if missing == nil {
		missing = []string{}
	}
	if err := menc.Encode(missing); err != nil {
		return nil, err
	}
	t, err := template.New("page").Parse(pageTemplate)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	title := "Test report"
	if r.Run.Suite != "" {
		title = r.Run.Suite + " test report"
	}
	err = t.Execute(&out, pageData{Title: title, Data: template.JS(data), Missing: template.JS(bytes.TrimSpace(mbuf.Bytes()))})
	return out.Bytes(), err
}
