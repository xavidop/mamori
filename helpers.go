package mamori

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
)

// VersionHash produces a stable version string from bytes. Provider authors can
// use it to synthesize a Value.Version when the backend has no native revision
// identifier, giving mamori cheap change detection without a byte comparison.
func VersionHash(b []byte) string {
	h := fnv.New64a()
	_, _ = h.Write(b)
	return strconv.FormatUint(h.Sum64(), 16)
}

// SelectKey extracts a single value from a structured payload, for refs of the
// form scheme://path#key.
//
// The fragment is interpreted one of two ways, chosen by its first character:
//
//   - A fragment beginning with '/' is an RFC 6901 JSON Pointer, addressing a
//     value at any depth, through objects and array elements alike:
//     "#/credentials/password", "#/replicas/5/host". Escapes are RFC 6901's:
//     "~1" for a literal '/', "~0" for a literal '~'.
//   - Any other fragment is a literal top-level key, exactly as it has always
//     been. This is what keeps "#ca.crt" addressing the key named "ca.crt"
//     rather than a path, which matters because "tls.crt"/"tls.key"/"ca.crt"
//     are the canonical Kubernetes TLS secret keys and dotted keys are the norm
//     in ConfigMaps and Java properties files.
//
// If key is empty, data is returned unchanged. String values are returned
// unquoted; objects, arrays, numbers, and booleans are returned as their JSON
// encoding, byte-for-byte as they appeared in the payload.
//
// An absent key or an out-of-range index wraps ErrNotFound, so the field's
// default: or optional handling applies. A structural mismatch (a pointer
// descending into a scalar, a non-numeric token against an array, a malformed
// escape, or a payload that is not JSON) wraps ErrInvalid instead, because it
// is a malformed request against this payload rather than an absence and must
// not be silently masked by a default.
//
// Provider authors should call SelectKey with ref.Key after fetching the raw
// payload, so that fragment selection behaves identically across all providers.
func SelectKey(data []byte, key string) ([]byte, error) {
	if key == "" {
		return data, nil
	}
	if strings.HasPrefix(key, "/") {
		return selectPointer(data, key)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, fmt.Errorf("mamori: cannot select key %q: payload is not a JSON object: %w: %w", key, ErrInvalid, err)
	}
	raw, ok := obj[key]
	if !ok {
		return nil, fmt.Errorf("mamori: key %q not present in payload: %w", key, ErrNotFound)
	}
	return unquoteJSON(raw), nil
}
