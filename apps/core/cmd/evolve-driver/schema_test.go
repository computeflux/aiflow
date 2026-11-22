package main

import (
	"encoding/json"
	"testing"
)

func validate(t *testing.T, s interface{ Validate(any) error }, raw string) error {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("test data not JSON: %v", err)
	}
	return s.Validate(v)
}

func TestSchemasCompileAndValidate(t *testing.T) {
	if err := validate(t, coderSchema, `{"ok":true,"diff":"a","files_changed":["x.py"],"session_id":"s"}`); err != nil {
		t.Fatalf("valid coder out rejected: %v", err)
	}
	if err := validate(t, coderSchema, `{"ok":"yes"}`); err == nil {
		t.Fatal("invalid coder out accepted (ok 非布尔)")
	}
	if err := validate(t, reviewerSchema, `{"ok":true,"comments":[{"level":"error","message":"m"}]}`); err != nil {
		t.Fatalf("valid reviewer out rejected: %v", err)
	}
	if err := validate(t, reviewerSchema, `{"ok":true,"comments":[{"level":"fatal","message":"m"}]}`); err == nil {
		t.Fatal("invalid reviewer out accepted (level 越界)")
	}
}
