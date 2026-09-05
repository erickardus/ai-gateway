package jsonx

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"unicode/utf8"
)

func TestPeek(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantModel  string
		wantStream bool
		wantHas    bool
	}{
		{"simple", `{"model":"claude-sonnet","stream":true}`, "claude-sonnet", true, true},
		{"no stream", `{"model":"gpt-5"}`, "gpt-5", false, false},
		{"stream false", `{"model":"m","stream":false}`, "m", false, true},
		{"order swapped", `{"stream":true,"model":"m"}`, "m", true, true},
		{
			name:      "nested model is ignored",
			body:      `{"messages":[{"model":"DECOY"}],"metadata":{"model":"DECOY2"},"model":"real"}`,
			wantModel: "real",
		},
		{
			name:      "model-like text inside a string value is ignored",
			body:      `{"system":"the \"model\": \"DECOY\" is fake","model":"real"}`,
			wantModel: "real",
		},
		{"absent", `{"messages":[]}`, "", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Peek([]byte(tc.body))
			if err != nil {
				t.Fatalf("Peek: %v", err)
			}
			if got.Model != tc.wantModel {
				t.Errorf("Model = %q, want %q", got.Model, tc.wantModel)
			}
			if got.Stream != tc.wantStream {
				t.Errorf("Stream = %v, want %v", got.Stream, tc.wantStream)
			}
			if got.HasStream != tc.wantHas {
				t.Errorf("HasStream = %v, want %v", got.HasStream, tc.wantHas)
			}
		})
	}
}

func TestPeekMalformed(t *testing.T) {
	if _, err := Peek([]byte(`{"model":`)); err == nil {
		t.Fatal("expected error on truncated JSON")
	}
}

func TestSetTopLevelStringPreservesEveryOtherByte(t *testing.T) {
	body := []byte(`{"model":"old-name","max_tokens":1024,"system":[{"type":"text","text":"keep  me","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	got, err := SetTopLevelString(body, "model", "new-upstream-id")
	if err != nil {
		t.Fatalf("SetTopLevelString: %v", err)
	}
	want := bytes.Replace(body, []byte(`"old-name"`), []byte(`"new-upstream-id"`), 1)
	if !bytes.Equal(got, want) {
		t.Errorf("bytes differ beyond the target value\n got: %s\nwant: %s", got, want)
	}
}

func TestSetTopLevelStringIgnoresNestedKeys(t *testing.T) {
	// A nested object carrying the same key name must not be rewritten.
	body := []byte(`{"messages":[{"model":"NESTED"}],"tools":[{"input_schema":{"model":"NESTED2"}}],"model":"TARGET"}`)
	got, err := SetTopLevelString(body, "model", "REPLACED")
	if err != nil {
		t.Fatalf("SetTopLevelString: %v", err)
	}
	if bytes.Contains(got, []byte(`"REPLACED"`)) == false {
		t.Fatal("target was not replaced")
	}
	if !bytes.Contains(got, []byte(`"NESTED"`)) || !bytes.Contains(got, []byte(`"NESTED2"`)) {
		t.Errorf("nested occurrences were modified: %s", got)
	}
	if bytes.Count(got, []byte("REPLACED")) != 1 {
		t.Errorf("expected exactly one replacement, got: %s", got)
	}
}

func TestSetTopLevelStringEscapes(t *testing.T) {
	// Escaped quotes and unicode escapes inside string values must not confuse
	// the scanner into mislocating the target.
	body := []byte(`{"a":"he said \"model\": \"x\"","b":"é😀","model":"t"}`)
	got, err := SetTopLevelString(body, "model", "z")
	if err != nil {
		t.Fatalf("SetTopLevelString: %v", err)
	}
	var check map[string]any
	if err := json.Unmarshal(got, &check); err != nil {
		t.Fatalf("result is not valid JSON: %v (%s)", err, got)
	}
	if check["model"] != "z" {
		t.Errorf("model = %v, want z", check["model"])
	}
	if check["a"] != `he said "model": "x"` {
		t.Errorf("sibling string was corrupted: %v", check["a"])
	}
}

func TestSetTopLevelStringValueNeedingEscape(t *testing.T) {
	got, err := SetTopLevelString([]byte(`{"model":"a"}`), "model", `we"ird\value`)
	if err != nil {
		t.Fatalf("SetTopLevelString: %v", err)
	}
	var check map[string]any
	if err := json.Unmarshal(got, &check); err != nil {
		t.Fatalf("result is not valid JSON: %v (%s)", err, got)
	}
	if check["model"] != `we"ird\value` {
		t.Errorf("model = %q", check["model"])
	}
}

func TestSetTopLevelStringErrors(t *testing.T) {
	if _, err := SetTopLevelString([]byte(`{"a":1}`), "model", "x"); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("absent key: got %v, want ErrKeyNotFound", err)
	}
	if _, err := SetTopLevelString([]byte(`{"model":123}`), "model", "x"); !errors.Is(err, ErrNotAString) {
		t.Errorf("numeric value: got %v, want ErrNotAString", err)
	}
	if _, err := SetTopLevelString([]byte(`{"model":{"a":1}}`), "model", "x"); !errors.Is(err, ErrNotAString) {
		t.Errorf("object value: got %v, want ErrNotAString", err)
	}
	if _, err := SetTopLevelString([]byte(`{"model":`), "model", "x"); err == nil {
		t.Error("truncated JSON: expected error")
	}
}

// FuzzSetTopLevelString asserts the splice never produces invalid JSON.
func FuzzSetTopLevelString(f *testing.F) {
	f.Add(`{"model":"a","b":[1,2,{"model":"c"}]}`, "repl")
	f.Add(`{"a":{"b":{"model":"deep"}},"model":"top"}`, "x\"y")
	f.Add(`{"model":"é"}`, "é")
	f.Fuzz(func(t *testing.T, body, repl string) {
		if !json.Valid([]byte(body)) {
			t.Skip()
		}
		got, err := SetTopLevelString([]byte(body), "model", repl)
		if err != nil {
			return
		}
		if !json.Valid(got) {
			t.Fatalf("produced invalid JSON\n in: %s\nout: %s", body, got)
		}
		var m map[string]any
		if err := json.Unmarshal(got, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		// encoding/json replaces invalid UTF-8 with U+FFFD, so an exact
		// round-trip is only guaranteed for well-formed input. The invariant
		// that must always hold is that the output is valid JSON.
		if utf8.ValidString(repl) && m["model"] != repl {
			t.Fatalf("model = %v, want %q", m["model"], repl)
		}
	})
}
