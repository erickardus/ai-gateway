package jsonx

import (
	"encoding/json"
	"testing"
)

// TestContainsObjectKeyDistinguishesKeysFromText is the reason this function
// exists: prompt text that mentions a key name is not the same as a request
// that carries it, and treating the two alike switches off an optimization for
// the whole of a conversation that happens to discuss caching.
func TestContainsObjectKeyDistinguishesKeysFromText(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want bool
	}{
		{
			name: "a real breakpoint",
			doc:  `{"system":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}`,
			want: true,
		},
		{
			name: "the name only as message text",
			doc:  `{"messages":[{"role":"user","content":"what does cache_control do?"}]}`,
			want: false,
		},
		{
			name: "the name quoted inside message text",
			doc:  `{"messages":[{"role":"user","content":"explain \"cache_control\" please"}]}`,
			want: false,
		},
		{
			name: "the name as a value rather than a key",
			doc:  `{"tools":[{"name":"cache_control"}]}`,
			want: false,
		},
		{
			name: "nested deep inside a tool result",
			doc:  `{"messages":[{"content":[{"type":"tool_result","content":[{"cache_control":{"type":"ephemeral"}}]}]}]}`,
			want: true,
		},
		{
			name: "escaped in the key itself",
			doc:  `{"system":[{"cache\u005fcontrol":{"type":"ephemeral"}}]}`,
			want: true,
		},
		{
			name: "a key that merely starts the same",
			doc:  `{"system":[{"cache_control_hint":true}]}`,
			want: false,
		},
		{
			name: "text containing a brace and a colon",
			doc:  `{"messages":[{"role":"user","content":"paste: {\"cache_control\": 1}"}]}`,
			want: false,
		},
		{
			name: "an empty document",
			doc:  `{}`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ContainsObjectKey([]byte(tt.doc), "cache_control")
			if err != nil {
				t.Fatalf("ContainsObjectKey: %v", err)
			}
			if got != tt.want {
				t.Errorf("ContainsObjectKey = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestContainsObjectKeyRejectsMalformedInput(t *testing.T) {
	if _, err := ContainsObjectKey([]byte(`{"a":`), "a"); err == nil {
		t.Error("want an error on malformed JSON, so the caller can choose its own safe answer")
	}
}

// FuzzContainsObjectKey checks the walk against the language's own reader: the
// two must agree on every document the reader accepts.
func FuzzContainsObjectKey(f *testing.F) {
	f.Add(`{"cache_control":1}`)
	f.Add(`{"a":[{"b":{"cache_control":{}}}]}`)
	f.Add(`{"a":"cache_control"}`)
	f.Add(`["cache_control"]`)
	f.Add(`{"a":"\"cache_control\":"}`)

	f.Fuzz(func(t *testing.T, doc string) {
		got, err := ContainsObjectKey([]byte(doc), "cache_control")
		if err != nil {
			return // malformed input is reported, not answered
		}
		if want := referenceHasKey([]byte(doc), "cache_control"); got != want {
			t.Fatalf("ContainsObjectKey = %v, want %v for %s", got, want, doc)
		}
	})
}

// referenceHasKey is a deliberately slow oracle built on the standard decoder,
// used only by the fuzzer. It answers the same question by materializing the
// document, which is exactly what the production path must not do.
func referenceHasKey(doc []byte, key string) bool {
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		return false
	}
	var walk func(any) bool
	walk = func(node any) bool {
		switch n := node.(type) {
		case map[string]any:
			if _, ok := n[key]; ok {
				return true
			}
			for _, child := range n {
				if walk(child) {
					return true
				}
			}
		case []any:
			for _, child := range n {
				if walk(child) {
					return true
				}
			}
		}
		return false
	}
	return walk(v)
}
