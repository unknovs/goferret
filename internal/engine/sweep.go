package engine

import (
	"os"
	"path/filepath"

	"github.com/unknovs/goferret/internal/results"
)

// sweep re-applies redaction to everything recorded during the run. A secret
// can be learned after a call was recorded (for example a token captured from
// that very response), so redaction at record time is not enough on its own.
func (r *Runner) sweep(res *results.Results) {
	s := r.Redact.String
	for i := range res.Calls {
		c := &res.Calls[i]
		c.URL, c.Error = s(c.URL), s(c.Error)
		c.ReqHeaders, c.ResHeaders = r.Redact.Headers(c.ReqHeaders), r.Redact.Headers(c.ResHeaders)
		c.ReqBody.Preview, c.ResBody.Preview = s(c.ReqBody.Preview), s(c.ResBody.Preview)
		for _, att := range []string{c.ReqBody.Attachment, c.ResBody.Attachment} {
			if att == "" || r.OutDir == "" {
				continue
			}
			p := filepath.Join(r.OutDir, filepath.FromSlash(att))
			if data, err := os.ReadFile(p); err == nil {
				if red := s(string(data)); red != string(data) {
					_ = os.WriteFile(p, []byte(red), 0o644)
				}
			}
		}
	}
	for i := range res.Scenarios {
		sc := &res.Scenarios[i]
		sc.Reason = s(sc.Reason)
		for j := range sc.Steps {
			st := &sc.Steps[j]
			st.Reason = s(st.Reason)
			for k := range st.Groups {
				for m := range st.Groups[k].Checks {
					ck := &st.Groups[k].Checks[m]
					ck.Desc, ck.Message = s(ck.Desc), s(ck.Message)
				}
			}
		}
	}
}
