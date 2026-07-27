package mamori

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// selectPointer resolves an RFC 6901 JSON Pointer against a JSON document and
// returns the addressed value, using the same encoding rule SelectKey applies
// to a literal key: a JSON string yields its unquoted contents, anything else
// yields its JSON encoding.
//
// The walk descends one level at a time over json.RawMessage rather than
// unmarshalling the whole document into an any tree. That preserves the exact
// bytes of a selected object or array: a marshal round trip would sort object
// keys and drop original whitespace, which would make a pointer-selected value
// differ byte-for-byte from a literally-selected one and perturb
// Value.changed's byte comparison for providers that supply no Version.
//
// ptr is guaranteed by the caller to begin with '/'.
func selectPointer(data []byte, ptr string) ([]byte, error) {
	cur := json.RawMessage(data)
	// A pointer's leading '/' produces an empty first token that is not a
	// reference token, so it is dropped.
	for _, raw := range strings.Split(ptr, "/")[1:] {
		tok, err := unescapeToken(raw)
		if err != nil {
			return nil, fmt.Errorf("mamori: pointer %q: %w", ptr, err)
		}
		next, err := descend(cur, tok, ptr)
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return unquoteJSON(cur), nil
}

// descend resolves one reference token against one node.
func descend(node json.RawMessage, tok, ptr string) (json.RawMessage, error) {
	switch containerKind(node) {
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(node, &obj); err != nil {
			return nil, fmt.Errorf("mamori: pointer %q: malformed object at token %q: %w: %w", ptr, tok, ErrInvalid, err)
		}
		v, ok := obj[tok]
		if !ok {
			return nil, fmt.Errorf("mamori: pointer %q: key %q not present: %w", ptr, tok, ErrNotFound)
		}
		return v, nil
	case '[':
		var arr []json.RawMessage
		if err := json.Unmarshal(node, &arr); err != nil {
			return nil, fmt.Errorf("mamori: pointer %q: malformed array at token %q: %w: %w", ptr, tok, ErrInvalid, err)
		}
		i, err := arrayIndex(tok)
		if err != nil {
			return nil, fmt.Errorf("mamori: pointer %q: %w", ptr, err)
		}
		if i >= len(arr) {
			return nil, fmt.Errorf("mamori: pointer %q: index %d out of range (array has %d elements): %w", ptr, i, len(arr), ErrNotFound)
		}
		return arr[i], nil
	default:
		// A scalar, or a payload that is not JSON at all. Either way the
		// pointer asks for something this document cannot contain: a
		// structural mismatch, not an absence, so default:/optional must not
		// mask it.
		return nil, fmt.Errorf("mamori: pointer %q: cannot descend into a non-container value at token %q: %w", ptr, tok, ErrInvalid)
	}
}

// containerKind reports the first significant byte of raw, which is '{' for an
// object, '[' for an array, and anything else (or 0 when empty) for a scalar or
// non-JSON payload.
func containerKind(raw json.RawMessage) byte {
	for _, c := range raw {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return c
		}
	}
	return 0
}

// unescapeToken applies RFC 6901's escaping: "~1" is '/', "~0" is '~'. The
// order matters and is why this is a single left-to-right pass rather than two
// strings.ReplaceAll calls: replacing "~0" first would turn the literal "~01"
// into "~1" and then into "/", which is wrong.
func unescapeToken(tok string) (string, error) {
	if !strings.ContainsRune(tok, '~') {
		return tok, nil
	}
	var b strings.Builder
	b.Grow(len(tok))
	for i := 0; i < len(tok); i++ {
		if tok[i] != '~' {
			b.WriteByte(tok[i])
			continue
		}
		if i+1 >= len(tok) {
			return "", fmt.Errorf("token %q ends with an unescaped %q: %w", tok, "~", ErrInvalid)
		}
		switch tok[i+1] {
		case '0':
			b.WriteByte('~')
		case '1':
			b.WriteByte('/')
		default:
			return "", fmt.Errorf("token %q contains invalid escape %q (want ~0 or ~1): %w", tok, tok[i:i+2], ErrInvalid)
		}
		i++
	}
	return b.String(), nil
}

// arrayIndex parses an RFC 6901 array index: either "0", or a non-zero digit
// followed by digits. A leading zero is rejected rather than silently accepted
// so that "/05" fails loudly instead of aliasing "/5". The "-" token, which
// RFC 6901 defines as one-past-the-end for JSON Patch's add operation, can
// never address an existing value and is rejected for the same reason.
func arrayIndex(tok string) (int, error) {
	switch {
	case tok == "-":
		return 0, fmt.Errorf("token %q addresses one past the end of the array and can never select a value: %w", tok, ErrInvalid)
	case tok == "":
		return 0, fmt.Errorf("empty array index token: %w", ErrInvalid)
	case len(tok) > 1 && tok[0] == '0':
		return 0, fmt.Errorf("array index %q has a leading zero: %w", tok, ErrInvalid)
	}
	i, err := strconv.Atoi(tok)
	if err != nil || i < 0 {
		return 0, fmt.Errorf("token %q is not an array index: %w", tok, ErrInvalid)
	}
	return i, nil
}

// unquoteJSON returns a JSON string's unquoted contents, or raw unchanged for
// any other JSON value. It is the shared final step of both SelectKey's literal
// path and selectPointer, so both encode a selected value identically.
func unquoteJSON(raw json.RawMessage) []byte {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []byte(s)
	}
	return raw
}
