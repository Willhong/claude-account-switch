// Package jsonpatch replaces a single top-level key in a JSON object without
// reserializing the rest of the document.
//
// ~/.claude.json is large and hand-inspectable; re-encoding it through Go's
// map marshaller would reorder every key on every `cas switch`. Splicing the
// one value we own leaves the remaining bytes untouched.
package jsonpatch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// SetTopLevelKey returns doc with key set to value (raw JSON). The key is
// created at the front of the object if it is not already present.
func SetTopLevelKey(doc []byte, key string, value []byte) ([]byte, error) {
	start, end, objOpen, found, err := locate(doc, key)
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer
	if found {
		out.Write(doc[:start])
		out.WriteString(": ")
		out.Write(value)
		out.Write(doc[end:])
		return out.Bytes(), nil
	}

	encKey, err := json.Marshal(key)
	if err != nil {
		return nil, err
	}
	out.Write(doc[:objOpen+1])
	out.WriteString("\n  ")
	out.Write(encKey)
	out.WriteString(": ")
	out.Write(value)
	// Only add a separating comma when the object already had members.
	if hasMembers(doc[objOpen+1:]) {
		out.WriteString(",")
	}
	out.Write(doc[objOpen+1:])
	return out.Bytes(), nil
}

// locate finds the byte span of key's value, excluding the key itself.
// start is the offset just after the closing quote of the key (so the colon and
// any whitespace fall inside the replaced span); end is just past the value.
func locate(doc []byte, key string) (start, end int64, objOpen int64, found bool, err error) {
	dec := json.NewDecoder(bytes.NewReader(doc))
	tok, err := dec.Token()
	if err != nil {
		return 0, 0, 0, false, fmt.Errorf("read JSON document: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return 0, 0, 0, false, fmt.Errorf("document is not a JSON object")
	}
	objOpen = dec.InputOffset() - 1

	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return 0, 0, 0, false, fmt.Errorf("read JSON key: %w", err)
		}
		name, ok := keyTok.(string)
		if !ok {
			return 0, 0, 0, false, fmt.Errorf("unexpected non-string JSON key")
		}
		afterKey := dec.InputOffset()

		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return 0, 0, 0, false, fmt.Errorf("read value of %q: %w", name, err)
		}
		if name == key {
			return afterKey, dec.InputOffset(), objOpen, true, nil
		}
	}
	if _, err := dec.Token(); err != nil && err != io.EOF {
		return 0, 0, 0, false, fmt.Errorf("read end of JSON object: %w", err)
	}
	return 0, 0, objOpen, false, nil
}

// hasMembers reports whether the remainder of an object body (starting just
// after '{') holds at least one member.
func hasMembers(rest []byte) bool {
	for _, b := range rest {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		case '}':
			return false
		default:
			return true
		}
	}
	return false
}

// MarshalIndentNoEscape encodes v with two-space indentation and without Go's
// default HTML escaping, matching how Claude Code writes ~/.claude.json.
func MarshalIndentNoEscape(v any, prefix string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent(prefix, "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
