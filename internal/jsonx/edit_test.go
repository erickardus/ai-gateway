package jsonx

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRawTopLevelReturnsValuesVerbatim(t *testing.T) {
	body := []byte(`{"model":"m","system":[{"type":"text","text":"a"}],"tools":[{"name":"t"}]}`)

	got, err := RawTopLevel(body, "system", "tools", "absent")
	if err != nil {
		t.Fatalf("RawTopLevel: %v", err)
	}
	if want := `[{"type":"text","text":"a"}]`; string(got[0]) != want {
		t.Errorf("system = %s, want %s", got[0], want)
	}
	if want := `[{"name":"t"}]`; string(got[1]) != want {
		t.Errorf("tools = %s, want %s", got[1], want)
	}
	if got[2] != nil {
		t.Errorf("absent key = %s, want nil", got[2])
	}
}

// A duplicate top-level key is refused here for the same reason Peek refuses
// one: two parsers may disagree about which occurrence wins.
func TestRawTopLevelRejectsDuplicateKeys(t *testing.T) {
	if _, err := RawTopLevel([]byte(`{"system":"a","system":"b"}`), "system"); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("err = %v, want ErrDuplicateKey", err)
	}
}

func TestSetTopLevelRawSplicesInPlace(t *testing.T) {
	body := []byte(`{"model":"m","system":"hello","stream":true}`)

	got, err := SetTopLevelRaw(body, "system", []byte(`[{"type":"text","text":"hello"}]`))
	if err != nil {
		t.Fatalf("SetTopLevelRaw: %v", err)
	}
	want := `{"model":"m","system":[{"type":"text","text":"hello"}],"stream":true}`
	if string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestSetTopLevelRawRejectsMissingKeyAndBadValue(t *testing.T) {
	body := []byte(`{"model":"m"}`)

	if _, err := SetTopLevelRaw(body, "system", []byte(`[]`)); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("missing key: err = %v, want ErrKeyNotFound", err)
	}
	if _, err := SetTopLevelRaw(body, "model", []byte(`{not json`)); err == nil {
		t.Error("invalid replacement: err = nil, want an error")
	}
}

func TestElements(t *testing.T) {
	tests := []struct {
		name  string
		array string
		want  []string
	}{
		{"empty", `[]`, nil},
		{"scalars", `[1, "two", null]`, []string{"1", `"two"`, "null"}},
		{"nested", `[{"a":[1,2]},{"b":"]"}]`, []string{`{"a":[1,2]}`, `{"b":"]"}`}},
		{"whitespace", "[\n  {\"a\":1}\n]", []string{`{"a":1}`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spans, err := Elements([]byte(tt.array))
			if err != nil {
				t.Fatalf("Elements: %v", err)
			}
			if len(spans) != len(tt.want) {
				t.Fatalf("got %d elements, want %d", len(spans), len(tt.want))
			}
			for i, span := range spans {
				if got := tt.array[span[0]:span[1]]; got != tt.want[i] {
					t.Errorf("element %d = %q, want %q", i, got, tt.want[i])
				}
			}
		})
	}
}

func TestElementsRejectsNonArrays(t *testing.T) {
	if _, err := Elements([]byte(`{"a":1}`)); !errors.Is(err, ErrNotAnArray) {
		t.Errorf("object: err = %v, want ErrNotAnArray", err)
	}
	if _, err := Elements([]byte(`[1,`)); err == nil {
		t.Error("invalid JSON: err = nil, want an error")
	}
}

func TestAddMember(t *testing.T) {
	tests := []struct {
		name   string
		object string
		want   string
		added  bool
	}{
		{"populated", `{"name":"t"}`, `{"name":"t","cache_control":{"type":"ephemeral"}}`, true},
		{"empty", `{}`, `{"cache_control":{"type":"ephemeral"}}`, true},
		{"spaced", `{ "a" : 1 }`, `{ "a" : 1,"cache_control":{"type":"ephemeral"} }`, true},
		{"already present", `{"cache_control":{"type":"ephemeral"}}`, `{"cache_control":{"type":"ephemeral"}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, added, err := AddMember([]byte(tt.object), "cache_control", []byte(`{"type":"ephemeral"}`))
			if err != nil {
				t.Fatalf("AddMember: %v", err)
			}
			if added != tt.added {
				t.Errorf("added = %v, want %v", added, tt.added)
			}
			if string(got) != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
			if !json.Valid(got) {
				t.Errorf("result is not valid JSON: %s", got)
			}
		})
	}
}

func TestAddMemberRejectsNonObjects(t *testing.T) {
	if _, _, err := AddMember([]byte(`[1]`), "k", []byte(`1`)); !errors.Is(err, ErrNotAnObject) {
		t.Errorf("array: err = %v, want ErrNotAnObject", err)
	}
	if _, _, err := AddMember([]byte(`{}`), "k", []byte(`oops`)); err == nil {
		t.Error("invalid value: err = nil, want an error")
	}
}

// Editing must leave every byte the gateway did not deliberately change exactly
// where it was: an upstream prompt cache keys on the request's bytes, so a
// reordered or renormalized body would miss the cache it was edited to hit.
func TestEditsPreserveSurroundingBytes(t *testing.T) {
	body := []byte("{\n  \"model\" : \"m\",\n  \"tools\" : [ {\"name\":\"t\"} ],\n  \"temperature\": 1.50\n}")

	values, err := RawTopLevel(body, "tools")
	if err != nil {
		t.Fatalf("RawTopLevel: %v", err)
	}
	spans, err := Elements(values[0])
	if err != nil {
		t.Fatalf("Elements: %v", err)
	}
	last := spans[len(spans)-1]
	marked, _, err := AddMember(values[0][last[0]:last[1]], "cache_control", []byte(`{"type":"ephemeral"}`))
	if err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	tools := append(append(append([]byte{}, values[0][:last[0]]...), marked...), values[0][last[1]:]...)

	got, err := SetTopLevelRaw(body, "tools", tools)
	if err != nil {
		t.Fatalf("SetTopLevelRaw: %v", err)
	}
	if !strings.Contains(string(got), `"temperature": 1.50`) {
		t.Errorf("unrelated number was renormalized: %s", got)
	}
	if !strings.HasPrefix(string(got), "{\n  \"model\" : \"m\",") {
		t.Errorf("unrelated whitespace changed: %s", got)
	}
	if !json.Valid(got) {
		t.Errorf("result is not valid JSON: %s", got)
	}
}

// The editors run on request bodies from the open internet, so no input may
// panic or produce something that is no longer JSON.
func FuzzEdit(f *testing.F) {
	f.Add(`{"system":"a","tools":[{"name":"t"}]}`)
	f.Add(`{"tools":[]}`)
	f.Add(`{}`)
	f.Add(`[1,2,3]`)
	f.Add(`{"a":{"b":[{"c":1}]}}`)

	f.Fuzz(func(t *testing.T, doc string) {
		body := []byte(doc)
		values, err := RawTopLevel(body, "tools")
		if err != nil {
			return
		}
		if values[0] != nil {
			if spans, err := Elements(values[0]); err == nil && len(spans) > 0 {
				last := spans[len(spans)-1]
				if elem := values[0][last[0]:last[1]]; len(elem) > 0 && elem[0] == '{' {
					marked, _, err := AddMember(elem, "cache_control", []byte(`{"type":"ephemeral"}`))
					if err != nil {
						t.Fatalf("AddMember on a valid object: %v", err)
					}
					if !json.Valid(marked) {
						t.Fatalf("AddMember produced invalid JSON: %s", marked)
					}
				}
			}
		}
		if out, err := SetTopLevelRaw(body, "tools", []byte(`[]`)); err == nil && !json.Valid(out) {
			t.Fatalf("SetTopLevelRaw produced invalid JSON: %s", out)
		}
	})
}
