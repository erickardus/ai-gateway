package jsonx

import (
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
