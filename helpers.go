package mamori

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strconv"
)

// VersionHash produces a stable version string from bytes. Provider authors can
// use it to synthesize a Value.Version when the backend has no native revision
// identifier, giving mamori cheap change detection without a byte comparison.
func VersionHash(b []byte) string {
	h := fnv.New64a()
	_, _ = h.Write(b)
	return strconv.FormatUint(h.Sum64(), 16)
}

// SelectKey extracts a single field named key from a JSON object payload, for
// refs of the form scheme://path#key. If key is empty, data is returned
// unchanged. String values are returned unquoted; objects, arrays, numbers, and
// booleans are returned as their JSON encoding. If the key is absent, SelectKey
// returns an error wrapping ErrNotFound.
//
// Provider authors should call SelectKey with ref.Key after fetching the raw
// payload, so that `#key` selection behaves identically across all providers.
func SelectKey(data []byte, key string) ([]byte, error) {
	if key == "" {
		return data, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, fmt.Errorf("mamori: cannot select key %q: payload is not a JSON object: %w", key, err)
	}
	raw, ok := obj[key]
	if !ok {
		return nil, fmt.Errorf("mamori: key %q not present in payload: %w", key, ErrNotFound)
	}
	// If the value is a JSON string, return the unquoted contents.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []byte(s), nil
	}
	return raw, nil
}
