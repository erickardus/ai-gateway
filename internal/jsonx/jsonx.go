// Package jsonx provides byte-preserving inspection and editing of JSON request
// bodies.
//
// The gateway must forward request bodies essentially unchanged. Anthropic's
// endpoint strips Claude Code's system-prompt attribution block positionally,
// which only works when the system array arrives exactly as sent, and prompt
// cache keys are sensitive to the body bytes. Unmarshalling into a map and
// re-marshalling would reorder object keys and renormalize numbers, so this
// package never does that: it locates a value and splices the replacement in,
// leaving every other byte identical.
package jsonx

import (
	"encoding/json"
	"errors"
	"fmt"
)

var (
	// ErrKeyNotFound means the requested top-level key is absent.
	ErrKeyNotFound = errors.New("top-level key not found")
	// ErrNotAString means the requested key exists but does not hold a string.
	ErrNotAString = errors.New("top-level key is not a string")
	// ErrDuplicateKey means a top-level key appears more than once. JSON parsers
	// disagree about which occurrence wins, so a body that relies on it would be
	// read one way here and another way upstream — a discrepancy that could let
	// an authorization check and the request it authorizes disagree about the
	// model being called.
	ErrDuplicateKey = errors.New("duplicate top-level key")
)

// Fields carries the few request fields the gateway needs in order to route.
// Everything else stays opaque and is forwarded untouched.
type Fields struct {
	Model     string
	Stream    bool
	HasStream bool
	// DisableFallbacks is a gateway control field. It is read here so the body
	// need not be parsed a second time, and stripped before forwarding: an
	// upstream that rejects unknown top-level fields would 400 the request.
	DisableFallbacks    bool
	HasDisableFallbacks bool

	// System, Tools and Messages carry the raw bytes of the fields a
	// prompt-cache prefix is derived from. They alias the body rather than
	// copying it, and are captured during the walk the router already performs
	// — deriving them separately would mean validating and scanning a
	// multi-megabyte document a second time on every request.
	System, Tools, Messages json.RawMessage
}

// LeadingMessages returns the raw bytes of up to n leading elements of the
// messages array, which are the turns that stay fixed as a conversation grows.
//
// It is a method on Fields rather than a free function because that is what
// carries the invariant it relies on: Peek validated the whole document, so
// these bytes are known to be well-formed and can be scanned without paying for
// validation again. Elements is the entry point for an array of unknown
// provenance.
func (f Fields) LeadingMessages(n int) [][]byte {
	if len(f.Messages) == 0 || n <= 0 {
		return nil
	}
	spans, err := elements(f.Messages)
	if err != nil {
		return nil
	}
	out := make([][]byte, 0, min(n, len(spans)))
	for _, span := range spans {
		if len(out) == n {
			break
		}
		out = append(out, f.Messages[span[0]:span[1]])
	}
	return out
}

// FirstMessageRole returns the role of the opening message, or "" where the
// request has none or does not name one.
//
// It reads the role alone rather than decoding the message, which may carry the
// largest system prompt in the request. Like LeadingMessages it relies on Peek
// having validated the document already, which is why it is a method here
// rather than a free function.
func (f Fields) FirstMessageRole() string {
	if len(f.Messages) == 0 {
		return ""
	}
	spans, err := elements(f.Messages)
	if err != nil || len(spans) == 0 {
		return ""
	}
	role := ""
	if err := walkObject(f.Messages[spans[0][0]:spans[0][1]], func(m member) error {
		if m.key != "role" {
			return nil
		}
		var s string
		if json.Unmarshal(m.raw, &s) == nil {
			role = s
		}
		return nil
	}); err != nil {
		return ""
	}
	return role
}

// member is one key/value pair of the root object.
type member struct {
	key string
	// raw is the value's bytes, exactly as they appear in the document.
	raw json.RawMessage
	// start and end bound the value within the original document.
	start, end int64
	// keyStart is the offset of the key's opening quote.
	keyStart int64
}

// Peek extracts the top-level fields the router needs, without materializing the
// rest of the document.
func Peek(body []byte) (Fields, error) {
	var out Fields
	err := walkTopLevel(body, func(m member) error {
		switch m.key {
		case "model":
			var s string
			if json.Unmarshal(m.raw, &s) == nil {
				out.Model = s
			}
		case "stream":
			var b bool
			if json.Unmarshal(m.raw, &b) == nil {
				out.Stream, out.HasStream = b, true
			}
		case "disable_fallbacks":
			var b bool
			if json.Unmarshal(m.raw, &b) == nil {
				out.DisableFallbacks, out.HasDisableFallbacks = b, true
			}
		case "system":
			out.System = m.raw
		case "tools":
			out.Tools = m.raw
		case "messages":
			out.Messages = m.raw
		}
		return nil
	})
	if err != nil {
		return Fields{}, fmt.Errorf("peek request body: %w", err)
	}
	return out, nil
}

// SetTopLevelString replaces the string value of a top-level key, leaving every
// other byte of the document identical. Only the root object's own keys are
// considered, so a nested object carrying the same key name is never touched.
func SetTopLevelString(body []byte, key, value string) ([]byte, error) {
	var start, end int64
	found, isString := false, false

	err := walkTopLevel(body, func(m member) error {
		if m.key != key {
			return nil
		}
		found = true
		start, end = m.start, m.end
		isString = len(m.raw) > 0 && m.raw[0] == '"'
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("set %q: %w", key, err)
	}
	if !found {
		return nil, fmt.Errorf("set %q: %w", key, ErrKeyNotFound)
	}
	if !isString {
		return nil, fmt.Errorf("set %q: %w", key, ErrNotAString)
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("set %q: encode replacement: %w", key, err)
	}

	out := make([]byte, 0, len(body)-int(end-start)+len(encoded))
	out = append(out, body[:start]...)
	out = append(out, encoded...)
	out = append(out, body[end:]...)
	return out, nil
}

// DeleteTopLevelKey removes a top-level key and its value, leaving the rest of
// the document byte-identical. It strips gateway control fields from a body
// before it is forwarded upstream.
func DeleteTopLevelKey(body []byte, key string) ([]byte, error) {
	var keyStart, valEnd int64
	found := false

	err := walkTopLevel(body, func(m member) error {
		if m.key != key {
			return nil
		}
		keyStart, valEnd, found = m.keyStart, m.end, true
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("delete %q: %w", key, err)
	}
	if !found {
		return body, nil
	}

	// Absorb the separating comma, whichever side of the pair it sits on.
	start, end := keyStart, valEnd
	for start > 0 && isJSONSpace(body[start-1]) {
		start--
	}
	if start > 0 && body[start-1] == ',' {
		start--
	} else {
		for int(end) < len(body) && isJSONSpace(body[end]) {
			end++
		}
		if int(end) < len(body) && body[end] == ',' {
			end++
		}
	}

	out := make([]byte, 0, len(body)-int(end-start))
	out = append(out, body[:start]...)
	out = append(out, body[end:]...)
	return out, nil
}

// walkTopLevel visits each member of the root object in order.
//
// Value extents are found by scanning rather than by decoding, so a nested array
// of messages or tool definitions is stepped over without being copied or
// tokenized. On a large request body that is the difference between touching a
// handful of keys and materializing megabytes the gateway never reads.
//
// The document is validated up front, which lets the scanner below assume
// well-formed input and stay simple.
func walkTopLevel(body []byte, visit func(member) error) error {
	if !json.Valid(body) {
		return errors.New("body is not valid JSON")
	}

	i := skipSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return errors.New("body is not a JSON object")
	}
	i++

	seen := make(map[string]bool)
	for {
		i = skipSpace(body, i)
		if i >= len(body) {
			return errors.New("unterminated object")
		}
		if body[i] == '}' {
			return nil
		}
		if body[i] == ',' {
			i++
			continue
		}
		if body[i] != '"' {
			return errors.New("malformed JSON: object key is not a string")
		}

		keyStart := int64(i)
		keyEnd, err := scanString(body, i)
		if err != nil {
			return err
		}
		var key string
		if err := json.Unmarshal(body[i:keyEnd], &key); err != nil {
			return fmt.Errorf("decode object key: %w", err)
		}
		if seen[key] {
			return fmt.Errorf("%q: %w", key, ErrDuplicateKey)
		}
		seen[key] = true

		i = skipSpace(body, keyEnd)
		if i >= len(body) || body[i] != ':' {
			return errors.New("malformed JSON: missing ':' after object key")
		}
		i = skipSpace(body, i+1)

		valEnd, err := scanValue(body, i)
		if err != nil {
			return err
		}
		if err := visit(member{
			key:      key,
			raw:      body[i:valEnd],
			start:    int64(i),
			end:      int64(valEnd),
			keyStart: keyStart,
		}); err != nil {
			return err
		}
		i = valEnd
	}
}

// skipSpace advances past JSON insignificant whitespace.
func skipSpace(b []byte, i int) int {
	for i < len(b) && isJSONSpace(b[i]) {
		i++
	}
	return i
}

// scanString returns the index just past a string that starts at b[i].
func scanString(b []byte, i int) (int, error) {
	if i >= len(b) || b[i] != '"' {
		return 0, errors.New("malformed JSON: expected a string")
	}
	for j := i + 1; j < len(b); j++ {
		switch b[j] {
		case '\\':
			j++ // skip the escaped byte
		case '"':
			return j + 1, nil
		}
	}
	return 0, errors.New("malformed JSON: unterminated string")
}

// scanValue returns the index just past the value starting at b[i]. The document
// is known to be valid, so this only needs to find the value's extent: it tracks
// container depth and string state, and steps over everything else.
func scanValue(b []byte, i int) (int, error) {
	if i >= len(b) {
		return 0, errors.New("malformed JSON: missing value")
	}

	switch b[i] {
	case '"':
		return scanString(b, i)
	case '{', '[':
		depth := 0
		for j := i; j < len(b); j++ {
			switch b[j] {
			case '"':
				end, err := scanString(b, j)
				if err != nil {
					return 0, err
				}
				j = end - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1, nil
				}
			}
		}
		return 0, errors.New("malformed JSON: unterminated container")
	default:
		// A number, or one of true/false/null: it ends at the first byte that
		// cannot continue a scalar.
		for j := i; j < len(b); j++ {
			switch b[j] {
			case ',', '}', ']', ' ', '\t', '\n', '\r':
				return j, nil
			}
		}
		return len(b), nil
	}
}

// isJSONSpace reports whether c is JSON insignificant whitespace.
func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
