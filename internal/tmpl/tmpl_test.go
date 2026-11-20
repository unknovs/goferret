package tmpl

import "testing"

func TestParse(t *testing.T) {
	p, err := Parse("a {{x}} b {{ $random_hex 8 }} c")
	if err != nil || len(p) != 5 || p[1].Var != "x" || p[3].Func != "random_hex" || p[3].Args[0] != "8" {
		t.Fatalf("%+v %v", p, err)
	}
	for _, bad := range []string{"{{x", "{{}}", "{{$nope 1}}", "{{$random_hex}}", "{{a b}}"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
	p, _ = Parse("plain")
	if len(p) != 1 || p[0].Lit != "plain" {
		t.Fatalf("%+v", p)
	}
}
