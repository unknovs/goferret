package jsonptr

import (
	"encoding/json"
	"testing"
)

func TestGet(t *testing.T) {
	var doc any
	json.Unmarshal([]byte(`{"a":{"b":[10,{"c":"x"}]},"k/l":1,"m~n":2,"":3}`), &doc)
	for _, tc := range []struct {
		p     string
		want  any
		found bool
	}{
		{"/a/b/0", float64(10), true},
		{"/a/b/1/c", "x", true},
		{"/k~1l", float64(1), true},
		{"/m~0n", float64(2), true},
		{"/", float64(3), true},
		{"/a/b/2", nil, false},
		{"/a/b/01", nil, false},
		{"/a/x", nil, false},
		{"/a/b/-", nil, false},
	} {
		v, found, err := Get(doc, tc.p)
		if err != nil || found != tc.found || (found && v != tc.want) {
			t.Errorf("%q: got %v %v %v", tc.p, v, found, err)
		}
	}
	if v, found, _ := Get(doc, ""); !found || v == nil {
		t.Error("empty pointer is the root")
	}
}

func TestValid(t *testing.T) {
	for _, bad := range []string{"a", "/a~", "/a~2", "x/y"} {
		if Valid(bad) == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
	for _, ok := range []string{"", "/", "/a/b", "/a~0b~1c"} {
		if err := Valid(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
}
