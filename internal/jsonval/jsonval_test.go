package jsonval

import (
	"encoding/json"
	"testing"
)

func TestText(t *testing.T) {
	cases := []struct{ got, want string }{
		{Text(nil), ""},
		{Text(true), "true"},
		{Text(json.Number("93.5")), "93.5"},
		{Text(10.0), "10"},
		{Text(0.1), "0.1"},
		{Text([]any{"a", json.Number("1"), nil}), `["a",1,null]`},
		{FormatFloat(1.5), "1.5"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

func TestOrderedMapKeepsKeyOrder(t *testing.T) {
	m := NewOrderedMap()
	m.Set("b", "<x>")
	m.Set("a", true)
	if got := JSON(m); got != `{"b":"<x>","a":true}` {
		t.Errorf("json: %s", got)
	}
}

func TestTruthyAndDecode(t *testing.T) {
	if !Truthy(json.Number("2")) || Truthy(json.Number("0")) || Truthy("") || Truthy([]any{}) || Truthy(nil) {
		t.Error("truthiness")
	}
	v, err := Decode([]byte(`{"n": 12345678901234567890}`))
	if err != nil || v.(map[string]any)["n"] != json.Number("12345678901234567890") {
		t.Errorf("numbers keep their exact text: %v %v", v, err)
	}
	if _, err := Decode([]byte(`{} {}`)); err == nil {
		t.Error("trailing data is an error")
	}
}
