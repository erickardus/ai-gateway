package jsonx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNotAnObject means a value that had to be a JSON object was something else.
var ErrNotAnObject = errors.New("value is not a JSON object")

// ErrNotAnArray means a value that had to be a JSON array was something else.
var ErrNotAnArray = errors.New("value is not a JSON array")

// RawTopLevel returns the raw bytes of each named top-level key, in the order
// requested. A key that is absent yields nil, which is distinguishable from a
// key holding JSON null only by inspecting the returned bytes.
//
// The results alias body rather than copying it: prompt-cache fingerprinting
// reads the system block, the tool definitions and the opening message on every
// request, and those are the largest values in the document.
func RawTopLevel(body []byte, keys ...string) ([][]byte, error) {
	out := make([][]byte, len(keys))
	err := walkTopLevel(body, func(m member) error {
		for i, k := range keys {
			if k == m.key {
				out[i] = m.raw
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read top-level keys: %w", err)
	}
	return out, nil
}

// SetTopLevelRaw replaces an existing top-level key's value with raw JSON,
// leaving every other byte of the document identical. The key must already be
// present: this edits a request, it does not extend one.
func SetTopLevelRaw(body []byte, key string, value []byte) ([]byte, error) {
	if !json.Valid(value) {
		return nil, fmt.Errorf("set %q: replacement is not valid JSON", key)
	}

	var start, end int64
	found := false
	err := walkTopLevel(body, func(m member) error {
		if m.key == key {
			start, end, found = m.start, m.end, true
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("set %q: %w", key, err)
	}
	if !found {
		return nil, fmt.Errorf("set %q: %w", key, ErrKeyNotFound)
	}

	out := make([]byte, 0, len(body)-int(end-start)+len(value))
	out = append(out, body[:start]...)
	out = append(out, value...)
	out = append(out, body[end:]...)
	return out, nil
}

// SetTopLevelValues replaces several top-level values in one pass, leaving every
// other byte of the document identical. Every named key must already be present:
// this edits a request, it does not extend one.
//
// It exists so that an edit touching two members costs one walk rather than two.
// Each walk revalidates the whole document, which on a request body carrying a
// large system prompt and a table of tool definitions is the dominant cost of
// editing it at all.
func SetTopLevelValues(body []byte, values map[string][]byte) ([]byte, error) {
	if len(values) == 0 {
		return body, nil
	}
	for key, value := range values {
		if !json.Valid(value) {
			return nil, fmt.Errorf("set %q: replacement is not valid JSON", key)
		}
	}

	// Collected in document order, which is the order walkTopLevel visits
	// members in, so the splice below can run straight through.
	type edit struct {
		start, end int64
		value      []byte
	}
	edits := make([]edit, 0, len(values))
	found := make(map[string]bool, len(values))
	err := walkTopLevel(body, func(m member) error {
		if value, ok := values[m.key]; ok {
			edits = append(edits, edit{m.start, m.end, value})
			found[m.key] = true
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("set top-level values: %w", err)
	}
	for key := range values {
		if !found[key] {
			return nil, fmt.Errorf("set %q: %w", key, ErrKeyNotFound)
		}
	}

	size := len(body)
	for _, e := range edits {
		size += len(e.value) - int(e.end-e.start)
	}
	out := make([]byte, 0, size)
	at := int64(0)
	for _, e := range edits {
		out = append(out, body[at:e.start]...)
		out = append(out, e.value...)
		at = e.end
	}
	return append(out, body[at:]...), nil
}

// Elements returns the extent of each element of a JSON array, as [start, end)
// offsets into array. An empty array yields no spans.
func Elements(array []byte) ([][2]int, error) {
	if !json.Valid(array) {
		return nil, errors.New("value is not valid JSON")
	}
	return elements(array)
}

// elements is the unvalidated core of Elements. It assumes well-formed input,
// which the exported wrapper establishes.
func elements(array []byte) ([][2]int, error) {
	i := skipSpace(array, 0)
	if i >= len(array) || array[i] != '[' {
		return nil, ErrNotAnArray
	}
	i++

	var spans [][2]int
	for {
		i = skipSpace(array, i)
		if i >= len(array) {
			return nil, errors.New("malformed JSON: unterminated array")
		}
		switch array[i] {
		case ']':
			return spans, nil
		case ',':
			i++
			continue
		}
		end, err := scanValue(array, i)
		if err != nil {
			return nil, err
		}
		spans = append(spans, [2]int{i, end})
		i = end
	}
}

// AddMember inserts a key and raw value into a JSON object, keeping every
// existing byte where it was. A key already present is left alone and the
// object is returned unchanged, reported by the second result.
//
// The member is appended rather than inserted in sorted position because the
// point of this package is that a forwarded body differs from the received one
// only where the gateway deliberately changed it.
func AddMember(object []byte, key string, value []byte) ([]byte, bool, error) {
	if !json.Valid(object) {
		return nil, false, errors.New("value is not valid JSON")
	}
	if !json.Valid(value) {
		return nil, false, fmt.Errorf("add %q: value is not valid JSON", key)
	}

	end := -1
	present := false
	err := walkObject(object, func(m member) error {
		if m.key == key {
			present = true
		}
		end = int(m.end)
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("add %q: %w", key, err)
	}
	if present {
		return object, false, nil
	}

	encodedKey, err := json.Marshal(key)
	if err != nil {
		return nil, false, fmt.Errorf("add %q: encode key: %w", key, err)
	}

	// Insert just after the last member's value, or just after the opening
	// brace when the object is empty. Either point is followed only by
	// whitespace and the closing brace.
	at := end
	if at < 0 {
		at = skipSpace(object, 0) + 1
	}

	out := make([]byte, 0, len(object)+len(encodedKey)+len(value)+2)
	out = append(out, object[:at]...)
	if end >= 0 {
		out = append(out, ',')
	}
	out = append(out, encodedKey...)
	out = append(out, ':')
	out = append(out, value...)
	out = append(out, object[at:]...)
	return out, true, nil
}

// HasMember reports whether an object carries a member with the given name.
//
// It looks only at the object's own members, which is what separates it from
// ContainsObjectKey: the question here is what kind of thing this object is, and
// a name buried in a nested schema does not answer that.
func HasMember(object []byte, key string) (bool, error) {
	if !json.Valid(object) {
		return false, errors.New("value is not valid JSON")
	}
	found := false
	if err := walkObject(object, func(m member) error {
		if m.key == key {
			found = true
		}
		return nil
	}); err != nil {
		return false, fmt.Errorf("read member %q: %w", key, err)
	}
	return found, nil
}

// walkObject visits each member of an object that is already known to be valid
// JSON. walkTopLevel is the same walk over a whole document; this one skips the
// validation so a nested object can be edited without re-scanning it.
func walkObject(object []byte, visit func(member) error) error {
	i := skipSpace(object, 0)
	if i >= len(object) || object[i] != '{' {
		return ErrNotAnObject
	}
	i++

	for {
		i = skipSpace(object, i)
		if i >= len(object) {
			return errors.New("malformed JSON: unterminated object")
		}
		switch object[i] {
		case '}':
			return nil
		case ',':
			i++
			continue
		case '"':
		default:
			return errors.New("malformed JSON: object key is not a string")
		}

		keyStart := int64(i)
		keyEnd, err := scanString(object, i)
		if err != nil {
			return err
		}
		var key string
		if err := json.Unmarshal(object[i:keyEnd], &key); err != nil {
			return fmt.Errorf("decode object key: %w", err)
		}

		i = skipSpace(object, keyEnd)
		if i >= len(object) || object[i] != ':' {
			return errors.New("malformed JSON: missing ':' after object key")
		}
		i = skipSpace(object, i+1)

		valEnd, err := scanValue(object, i)
		if err != nil {
			return err
		}
		if err := visit(member{
			key:      key,
			raw:      object[i:valEnd],
			start:    int64(i),
			end:      int64(valEnd),
			keyStart: keyStart,
		}); err != nil {
			return err
		}
		i = valEnd
	}
}

// ContainsObjectKey reports whether any object anywhere in a JSON value has a
// member with the given name.
//
// It exists because a substring search does not answer that question. Prompt
// text is JSON string data, and a request whose conversation happens to discuss
// `cache_control` — a developer asking Claude Code about prompt caching, a diff
// that touches this very file — contains those bytes without carrying a
// breakpoint. Treating that as "the caller manages its own breakpoints" silently
// switches injection off for the rest of the conversation, which is a bill
// nobody can trace back to a word in a message.
//
// The walk distinguishes a key from a value by position rather than by content:
// only a string in key position inside an object counts. The document must be
// valid JSON; malformed input is reported as an error rather than a false
// negative, because the caller's safe answer is "assume it is there".
func ContainsObjectKey(doc []byte, key string) (bool, error) {
	if !json.Valid(doc) {
		return false, errors.New("value is not valid JSON")
	}

	// Object depth is tracked as a bit per level: a key may only appear where
	// the innermost container is an object.
	inObject := make([]bool, 0, 32)
	expectKey := false

	for i := 0; i < len(doc); {
		switch c := doc[i]; c {
		case '{':
			inObject = append(inObject, true)
			expectKey = true
			i++
		case '[':
			inObject = append(inObject, false)
			expectKey = false
			i++
		case '}', ']':
			if len(inObject) == 0 {
				return false, errors.New("malformed JSON: unbalanced container")
			}
			inObject = inObject[:len(inObject)-1]
			expectKey = false
			i++
		case '"':
			end, err := scanString(doc, i)
			if err != nil {
				return false, err
			}
			if expectKey {
				if matchesKey(doc[i:end], key) {
					return true, nil
				}
				expectKey = false
			}
			i = end
		case ',':
			expectKey = len(inObject) > 0 && inObject[len(inObject)-1]
			i++
		case ':':
			expectKey = false
			i++
		default:
			i++
		}
	}
	return false, nil
}

// matchesKey compares a quoted JSON string against a plain key name.
//
// The common case is a byte comparison against the obvious encoding. Anything
// carrying an escape is decoded first, because a key written as
// "cache\u005fcontrol" names the same member as "cache_control", and an
// upstream reading the request would treat the two as one.
func matchesKey(quoted []byte, key string) bool {
	if len(quoted) == len(key)+2 && string(quoted[1:len(quoted)-1]) == key {
		return true
	}
	if !bytes.ContainsRune(quoted, '\\') {
		return false
	}
	var decoded string
	if err := json.Unmarshal(quoted, &decoded); err != nil {
		return false
	}
	return decoded == key
}

// StripObjectKey removes every member named key, wherever it appears in doc,
// and reports whether anything was removed. Every other byte is left exactly
// where it was.
//
// It exists for prompt-cache fingerprinting, where the question is whether two
// requests carry the same prompt rather than whether they are the same
// document. A cache breakpoint is metadata about where a prefix ends, not part
// of the prompt an upstream tokenizes: a client that walks its breakpoint
// forward as a conversation grows — which is what Claude Code and the SDKs both
// do — is sending the same prefix each turn, and the provider treats it as one.
// Hashing the raw bytes would call each turn a different prefix and scatter the
// conversation across deployments, paying a cache write per turn.
//
// The walk is structural rather than textual for the same reason
// ContainsObjectKey is: prompt text discussing cache_control contains those
// bytes without carrying a member of that name.
func StripObjectKey(doc []byte, key string) ([]byte, bool, error) {
	if !json.Valid(doc) {
		return nil, false, errors.New("value is not valid JSON")
	}
	var spans [][2]int
	if err := collectMembers(doc, key, 0, &spans); err != nil {
		return nil, false, err
	}
	if len(spans) == 0 {
		return doc, false, nil
	}
	return spliceOut(doc, spans), true, nil
}

// collectMembers appends the extent of every member named key within value, as
// offsets into the document value was taken from. base is where value sits in
// that document, so nested spans come back in the caller's coordinates.
//
// Members are visited in document order and recursion into a value happens
// before the next member is read, so the spans accumulate already sorted, which
// is what lets spliceOut run straight through them.
func collectMembers(value []byte, key string, base int, out *[][2]int) error {
	i := skipSpace(value, 0)
	if i >= len(value) {
		return nil
	}
	switch value[i] {
	case '{':
		return walkObject(value[i:], func(m member) error {
			if m.key == key {
				*out = append(*out, [2]int{base + i + int(m.keyStart), base + i + int(m.end)})
				return nil
			}
			return collectMembers(m.raw, key, base+i+int(m.start), out)
		})
	case '[':
		spans, err := elements(value[i:])
		if err != nil {
			return err
		}
		for _, s := range spans {
			if err := collectMembers(value[i+s[0]:i+s[1]], key, base+i+s[0], out); err != nil {
				return err
			}
		}
	}
	// A scalar holds no members. A string in particular is prompt text, which is
	// exactly what must not be searched for a key name.
	return nil
}

// MemberValues returns the raw value of every member named key, wherever it
// appears in doc, in document order.
//
// It answers what those members say, where StripObjectKey removes them and
// ContainsObjectKey only reports that one exists. Prompt-cache pinning uses it
// to read the lifetime a caller asked its breakpoints to have, which is a few
// dozen bytes inside a request that may be megabytes.
func MemberValues(doc []byte, key string) ([][]byte, error) {
	if !json.Valid(doc) {
		return nil, errors.New("value is not valid JSON")
	}
	var out [][]byte
	if err := visitMembers(doc, key, func(raw []byte) { out = append(out, raw) }); err != nil {
		return nil, err
	}
	return out, nil
}

// visitMembers calls fn with the value of every member named key within value.
// It shares collectMembers' walk and differs only in what it keeps: the value
// rather than the extent to cut out.
func visitMembers(value []byte, key string, fn func([]byte)) error {
	i := skipSpace(value, 0)
	if i >= len(value) {
		return nil
	}
	switch value[i] {
	case '{':
		return walkObject(value[i:], func(m member) error {
			if m.key == key {
				fn(m.raw)
				return nil
			}
			return visitMembers(m.raw, key, fn)
		})
	case '[':
		spans, err := elements(value[i:])
		if err != nil {
			return err
		}
		for _, s := range spans {
			if err := visitMembers(value[i+s[0]:i+s[1]], key, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// spliceOut removes the given ascending, non-overlapping spans from doc, taking
// one adjoining comma with each so the result is still valid JSON.
//
// The comma before the member is preferred, since it is the one that cannot
// belong to a member that survives. Where there is none — the member opens its
// object, or its predecessor was itself removed — the comma after it is taken
// instead. Clamping each span to where the previous one ended is what keeps two
// removed neighbours from consuming the same separator twice.
func spliceOut(doc []byte, spans [][2]int) []byte {
	out := make([]byte, 0, len(doc))
	at := 0
	for _, s := range spans {
		start, end := s[0], s[1]
		lo := start
		for lo > at && isJSONSpace(doc[lo-1]) {
			lo--
		}
		if lo > at && doc[lo-1] == ',' {
			start = lo - 1
		} else {
			for end < len(doc) && isJSONSpace(doc[end]) {
				end++
			}
			if end < len(doc) && doc[end] == ',' {
				end++
			}
		}
		if start < at {
			start = at
		}
		out = append(out, doc[at:start]...)
		at = end
	}
	return append(out, doc[at:]...)
}
