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
		out, err := SetTopLevelValues(body, map[string][]byte{"tools": []byte(`[]`), "system": []byte(`""`)})
		if err == nil && !json.Valid(out) {
			t.Fatalf("SetTopLevelValues produced invalid JSON: %s", out)
		}
	})
}

func TestSetTopLevelValues(t *testing.T) {
	body := []byte(`{"model":"m","system":"s","temperature":1.50,"tools":[{"a":1}],"stream":true}`)

	got, err := SetTopLevelValues(body, map[string][]byte{
		"tools":  []byte(`[{"a":1,"marked":true}]`),
		"system": []byte(`[{"type":"text","text":"s"}]`),
	})
	if err != nil {
		t.Fatalf("SetTopLevelValues: %v", err)
	}
	want := `{"model":"m","system":[{"type":"text","text":"s"}],"temperature":1.50,"tools":[{"a":1,"marked":true}],"stream":true}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// The replacements are applied wherever their keys sit, not in the order they
// were handed over — a map has no order to inherit.
func TestSetTopLevelValuesIgnoresArgumentOrder(t *testing.T) {
	body := []byte(`{"a":1,"b":2,"c":3}`)

	first, err := SetTopLevelValues(body, map[string][]byte{"c": []byte(`"z"`), "a": []byte(`"x"`)})
	if err != nil {
		t.Fatalf("SetTopLevelValues: %v", err)
	}
	if want := `{"a":"x","b":2,"c":"z"}`; string(first) != want {
		t.Errorf("got %s, want %s", first, want)
	}
}

func TestSetTopLevelValuesRejectsMissingKeysAndBadValues(t *testing.T) {
	body := []byte(`{"model":"m"}`)

	// Nothing is written when any key is absent, so a partial edit cannot reach
	// an upstream.
	if _, err := SetTopLevelValues(body, map[string][]byte{
		"model": []byte(`"n"`), "system": []byte(`[]`),
	}); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("missing key: err = %v, want ErrKeyNotFound", err)
	}
	if _, err := SetTopLevelValues(body, map[string][]byte{"model": []byte(`{oops`)}); err == nil {
		t.Error("invalid replacement: err = nil, want an error")
	}
	if got, err := SetTopLevelValues(body, nil); err != nil || string(got) != string(body) {
		t.Errorf("empty edit: got %s, %v; want the body unchanged", got, err)
	}
}

// One walk for two edits must produce exactly what two separate walks did.
func TestSetTopLevelValuesMatchesRepeatedSingleEdits(t *testing.T) {
	body := []byte("{\n  \"system\" : \"s\",\n  \"n\": 1.50,\n  \"tools\" : [ {\"a\":1} ]\n}")
	system, tools := []byte(`["s"]`), []byte(`[{"a":2}]`)

	once, err := SetTopLevelValues(body, map[string][]byte{"system": system, "tools": tools})
	if err != nil {
		t.Fatalf("SetTopLevelValues: %v", err)
	}
	twice, err := SetTopLevelRaw(body, "system", system)
	if err != nil {
		t.Fatalf("SetTopLevelRaw: %v", err)
	}
	if twice, err = SetTopLevelRaw(twice, "tools", tools); err != nil {
		t.Fatalf("SetTopLevelRaw: %v", err)
	}
	if string(once) != string(twice) {
		t.Errorf("one pass gave  %s\ntwo passes gave %s", once, twice)
	}
	if !strings.Contains(string(once), `"n": 1.50`) {
		t.Errorf("an unrelated number was renormalized: %s", once)
	}
}

// TestStripObjectKey covers the removals prompt-cache fingerprinting depends on.
// The result must stay valid JSON and must differ from the input only by the
// members named, since anything else would make two identical prompts hash
// differently.
func TestStripObjectKey(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		removed bool
	}{
		{"absent", `{"a":1}`, `{"a":1}`, false},
		{"only member", `{"cc":{"type":"ephemeral"}}`, `{}`, true},
		{"last member", `{"a":1,"cc":{"type":"ephemeral"}}`, `{"a":1}`, true},
		{"first member", `{"cc":1,"a":2}`, `{"a":2}`, true},
		{"middle member", `{"a":1,"cc":2,"b":3}`, `{"a":1,"b":3}`, true},
		{"adjacent members", `{"cc":1,"cc":2}`, `{}`, true},
		{"nested in array", `{"content":[{"text":"hi","cc":{"ttl":"1h"}}]}`, `{"content":[{"text":"hi"}]}`, true},
		{
			"several depths",
			`{"system":[{"text":"s","cc":1}],"messages":[{"content":[{"text":"m","cc":2}]}]}`,
			`{"system":[{"text":"s"}],"messages":[{"content":[{"text":"m"}]}]}`,
			true,
		},
		{"whitespace around member", `{"a":1 , "cc" : 2 , "b":3}`, `{"a":1  , "b":3}`, true},
		{"top-level array", `[{"cc":1,"a":2},{"a":3}]`, `[{"a":2},{"a":3}]`, true},
		// The name in string data is prompt text, not a member: a conversation
		// about prompt caching must not be edited by this.
		{"name as a value", `{"a":"cc","b":"the cc member"}`, `{"a":"cc","b":"the cc member"}`, false},
		{"name as escaped key", `{"a":1,"\u0063c":2}`, `{"a":1}`, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, removed, err := StripObjectKey([]byte(tc.in), "cc")
			if err != nil {
				t.Fatalf("StripObjectKey: %v", err)
			}
			if removed != tc.removed {
				t.Errorf("removed = %v, want %v", removed, tc.removed)
			}
			if string(got) != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
			if !json.Valid(got) {
				t.Errorf("result is not valid JSON: %s", got)
			}
		})
	}
}

// TestStripObjectKeyIsStableUnderPlacement is the property the fingerprint
// rests on: two documents differing only in where a member sits strip to the
// same bytes.
func TestStripObjectKeyIsStableUnderPlacement(t *testing.T) {
	first := `{"messages":[{"content":[{"text":"a","cc":1}]},{"content":[{"text":"b"}]}]}`
	later := `{"messages":[{"content":[{"text":"a"}]},{"content":[{"text":"b","cc":1}]}]}`

	a, _, err := StripObjectKey([]byte(first), "cc")
	if err != nil {
		t.Fatalf("StripObjectKey: %v", err)
	}
	b, _, err := StripObjectKey([]byte(later), "cc")
	if err != nil {
		t.Fatalf("StripObjectKey: %v", err)
	}
	if string(a) != string(b) {
		t.Errorf("a moved member changed the stripped document:\n %s\n %s", a, b)
	}
}

// FuzzStripObjectKey holds the two properties the fingerprint relies on against
// arbitrary input: the result is still a document, and the member is gone from
// it. A splice that miscounted a comma would break the first; one that missed a
// nesting level would break the second, and the only symptom in production would
// be a conversation quietly re-pinned every turn.
func FuzzStripObjectKey(f *testing.F) {
	for _, seed := range []string{
		`{}`,
		`{"a":1}`,
		`{"cc":1}`,
		`{"a":1,"cc":{"type":"ephemeral"},"b":2}`,
		`{"system":[{"text":"s","cc":1}],"messages":[{"content":[{"text":"m","cc":2}]}]}`,
		`[{"cc":1},{"cc":2}]`,
		`{"a":"cc"}`,
		`{"a":{"b":{"c":{"cc":[1,2,{"cc":3}]}}}}`,
		`{"a":1 , "cc" : 2 , "b":3}`,
		`"cc"`,
		`null`,
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, doc string) {
		if !json.Valid([]byte(doc)) {
			return
		}
		got, removed, err := StripObjectKey([]byte(doc), "cc")
		if err != nil {
			t.Fatalf("StripObjectKey on valid JSON: %v", err)
		}
		if !json.Valid(got) {
			t.Fatalf("result is not valid JSON\n in: %s\nout: %s", doc, got)
		}
		present, err := ContainsObjectKey(got, "cc")
		if err != nil {
			t.Fatalf("ContainsObjectKey: %v", err)
		}
		if present {
			t.Fatalf("the member survived stripping\n in: %s\nout: %s", doc, got)
		}
		if !removed && string(got) != doc {
			t.Fatalf("nothing was removed but the document changed\n in: %s\nout: %s", doc, got)
		}
		if len(got) > len(doc) {
			t.Fatalf("stripping made the document longer\n in: %s\nout: %s", doc, got)
		}
		// Stripping twice must reach the same place as stripping once, or the
		// fingerprint would depend on how many times a body had been handled.
		again, removedAgain, err := StripObjectKey(got, "cc")
		if err != nil {
			t.Fatalf("second StripObjectKey: %v", err)
		}
		if removedAgain || string(again) != string(got) {
			t.Fatalf("stripping is not idempotent\n once: %s\ntwice: %s", got, again)
		}
	})
}
