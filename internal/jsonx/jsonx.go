// Package jsonx provides byte-preserving inspection and editing of JSON request
// bodies.
//
// The gateway must forward request bodies essentially unchanged. Anthropic's
// endpoint strips Claude Code's system-prompt attribution block positionally,
// which only works when the system array arrives exactly as sent, and prompt
// cache keys are sensitive to the body bytes. Unmarshalling into a map and
// re-marshalling would reorder object keys and renormalize numbers, so this
// package never does that: it locates a value with a real JSON scanner and
// splices the replacement in, leaving every other byte identical.
package jsonx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrKeyNotFound means the requested top-level key is absent.
var ErrKeyNotFound = errors.New("top-level key not found")

// ErrNotAString means the requested key exists but does not hold a string.
var ErrNotAString = errors.New("top-level key is not a string")

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
}

// Peek extracts the top-level "model" and "stream" fields without materializing
// the rest of the document. Nested occurrences of those names are ignored.
func Peek(body []byte) (Fields, error) {
	var out Fields
	err := walkTopLevel(body, func(key string, value json.Token, _, _ int64) (bool, error) {
		switch key {
		case "model":
			if s, ok := value.(string); ok {
				out.Model = s
			}
		case "stream":
			if b, ok := value.(bool); ok {
				out.Stream, out.HasStream = b, true
			}
		case "disable_fallbacks":
			if b, ok := value.(bool); ok {
				out.DisableFallbacks, out.HasDisableFallbacks = b, true
			}
		}
		return false, nil
	})
	if err != nil {
		return Fields{}, fmt.Errorf("peek request body: %w", err)
	}
	return out, nil
}

// SetTopLevelString replaces the string value of a top-level key, leaving every
// other byte of the document identical. Only the root object's own keys are
// considered, so a nested object that happens to contain the same key name is
// never touched.
func SetTopLevelString(body []byte, key, value string) ([]byte, error) {
	var valStart, valEnd int64
	isString := false
	found := false

	// Do not stop at the first match. encoding/json — and therefore every
	// upstream and this package's own Peek — resolves a duplicated top-level
	// key to its LAST occurrence, so rewriting the first would leave the
	// caller's original value authoritative and silently defeat the rewrite.
	err := walkTopLevel(body, func(k string, v json.Token, afterKey, end int64) (bool, error) {
		if k != key {
			return false, nil
		}
		found = true
		if _, ok := v.(string); !ok {
			isString = false
			return false, nil
		}
		isString = true
		valEnd = end
		// Between the end of the key and the start of its value there is only a
		// colon and optional whitespace, so the first quote after the key opens
		// the value.
		rel := bytes.IndexByte(body[afterKey:end], '"')
		if rel < 0 {
			isString = false
			return false, nil
		}
		valStart = afterKey + int64(rel)
		return false, nil
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

	out := make([]byte, 0, len(body)-int(valEnd-valStart)+len(encoded))
	out = append(out, body[:valStart]...)
	out = append(out, encoded...)
	out = append(out, body[valEnd:]...)
	return out, nil
}

// visitFn is called for each key of the root object. value is the decoded scalar
// value, or nil when the value is a nested object or array. afterKey and valEnd
// bound the region of the document between the end of the key token and the end
// of the value, which is where a scalar value's raw bytes lie. Returning true
// stops the walk.
type visitFn func(key string, value json.Token, afterKey, valEnd int64) (bool, error)

// walkTopLevel scans a JSON document, invoking visit for each key of the root
// object. It tracks container nesting itself and reads values itself, so keys
// inside nested objects and arrays are never mistaken for top-level ones and the
// nesting state cannot be corrupted by a callback.
func walkTopLevel(body []byte, visit visitFn) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()

	// stack records enclosing containers; expectKey is true when the next token
	// in the innermost container is an object key rather than a value.
	var stack []json.Delim
	expectKey := false

	push := func(d json.Delim) {
		stack = append(stack, d)
		expectKey = d == '{'
	}

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}

		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				push(d)
			case '}', ']':
				if len(stack) == 0 {
					return errors.New("malformed JSON: unbalanced delimiter")
				}
				stack = stack[:len(stack)-1]
				expectKey = len(stack) > 0 && stack[len(stack)-1] == '{'
			}
			continue
		}

		inObject := len(stack) > 0 && stack[len(stack)-1] == '{'
		if !expectKey || !inObject {
			// A scalar value. Inside an object the next token is a key again.
			expectKey = inObject
			continue
		}

		key, ok := tok.(string)
		if !ok {
			return errors.New("malformed JSON: object key is not a string")
		}
		afterKey := dec.InputOffset()

		if len(stack) != 1 {
			// A key in a nested object: skip it and let its value be handled by
			// the normal value path on the next iteration.
			expectKey = false
			continue
		}

		// Root-level key. Read its value here so that nesting state stays
		// consistent no matter what the callback does.
		vtok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := vtok.(json.Delim); ok {
			push(d)
			if stop, err := visit(key, nil, afterKey, dec.InputOffset()); err != nil || stop {
				return err
			}
			continue
		}
		valEnd := dec.InputOffset()
		if stop, err := visit(key, vtok, afterKey, valEnd); err != nil || stop {
			return err
		}
		// Back to expecting the next key of the root object.
		expectKey = true
	}
}

// DeleteTopLevelKey removes a top-level key and its value, leaving the rest of
// the document byte-identical. It is used to strip gateway control fields from a
// body before it is forwarded upstream.
func DeleteTopLevelKey(body []byte, key string) ([]byte, error) {
	var keyStart, valEnd int64
	found := false

	err := walkTopLevel(body, func(k string, _ json.Token, afterKey, end int64) (bool, error) {
		if k != key {
			return false, nil
		}
		// Walk back from the key token to its opening quote.
		rel := bytes.LastIndexByte(body[:afterKey-1], '"')
		if rel < 0 {
			return false, nil
		}
		keyStart, valEnd, found = int64(rel), end, true
		return false, nil
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
	switch {
	case start > 0 && body[start-1] == ',':
		start--
	default:
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

// isJSONSpace reports whether c is JSON insignificant whitespace.
func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
