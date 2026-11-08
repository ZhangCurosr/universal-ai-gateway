package jsonx

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestSetTopLevelPreservesInnerBytes(t *testing.T) {
	// 内部带有不规则空白与大整数，改写 model 后必须原样保留
	body := []byte(`{"model":"a","messages":[ {"role":"user","content":"<b>hi</b> & bye","cache_control":{"type":"ephemeral"}} ],"n":12345678901234567890}`)
	out, err := SetTopLevelString(body, "model", "b")
	if err != nil {
		t.Fatalf("SetTopLevelString() error = %v", err)
	}
	if got, _ := GetString(out, "model"); got != "b" {
		t.Fatalf("model = %q, want b", got)
	}
	inner := []byte(`[ {"role":"user","content":"<b>hi</b> & bye","cache_control":{"type":"ephemeral"}} ]`)
	if !json.Valid(out) {
		t.Fatalf("output is not valid JSON: %s", out)
	}
	if !bytes.Contains(out, inner) {
		t.Fatalf("messages bytes changed: %s", out)
	}
	if !bytes.Contains(out, []byte("12345678901234567890")) {
		t.Fatalf("big number changed: %s", out)
	}
}

func TestSetTopLevelRejectsNonObject(t *testing.T) {
	for _, in := range []string{``, `[]`, `"x"`, `{bad`} {
		if _, err := SetTopLevelString([]byte(in), "model", "b"); err == nil {
			t.Errorf("SetTopLevelString(%q) error = nil, want error", in)
		}
	}
}

func TestGetters(t *testing.T) {
	body := []byte(`{"stream":true,"max_tokens":100,"metadata":{"user_id":"u1"},"x":null,"arr":[1,2]}`)
	if v, ok := GetBool(body, "stream"); !ok || !v {
		t.Errorf("GetBool(stream) = %v, %v", v, ok)
	}
	if v, ok := GetInt64(body, "max_tokens"); !ok || v != 100 {
		t.Errorf("GetInt64(max_tokens) = %v, %v", v, ok)
	}
	if v, ok := GetString(body, "metadata", "user_id"); !ok || v != "u1" {
		t.Errorf("GetString(metadata.user_id) = %q, %v", v, ok)
	}
	if _, ok := Get(body, "x"); ok {
		t.Error("Get(null field) ok = true, want false")
	}
	if _, ok := Get(body, "missing", "deep"); ok {
		t.Error("Get(missing path) ok = true, want false")
	}
	arr, _ := Get(body, "arr")
	if e, ok := ArrayElem(arr, 1); !ok || string(e) != "2" {
		t.Errorf("ArrayElem(arr,1) = %s, %v", e, ok)
	}
	if _, ok := ArrayElem(arr, 5); ok {
		t.Error("ArrayElem out of range ok = true")
	}
	if !json.Valid(arr) {
		t.Error("raw array invalid")
	}
}

func TestObjectAndIsNull(t *testing.T) {
	m, err := Object([]byte(` {"a": [1, 2], "b": null} `))
	if err != nil || string(m["a"]) != "[1, 2]" || !IsNull(m["b"]) || !IsNull(m["missing"]) || IsNull(m["a"]) {
		t.Fatalf("Object() = %v, %v", m, err)
	}
	if _, err := Object([]byte(`[1]`)); err != ErrNotObject {
		t.Fatalf("Object(array) error = %v", err)
	}
}
