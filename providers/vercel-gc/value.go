package vercelgc

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/xavidop/mamori"
)

// jsonRaw is a stored Global Config value, kept as raw JSON so no numeric
// precision is lost on the way through.
type jsonRaw = json.RawMessage

// rawToBytes converts a stored JSON value to the bytes mamori decodes from.
//
// Only a JSON string is unwrapped, to its raw text without quotes or escapes.
// Every other type passes through as its own compacted JSON encoding, which
// makes a number exactly what the store holds rather than what a float64
// round trip would produce.
//
// A key stored as null exists, so it yields the four bytes "null". Only an
// absent key is not-found.
func rawToBytes(raw jsonRaw) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("mamori/vercel-gc: empty value: %w", mamori.ErrInvalid)
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, fmt.Errorf("mamori/vercel-gc: decoding string value: %w: %w", mamori.ErrInvalid, err)
		}
		return []byte(s), nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, trimmed); err != nil {
		return nil, fmt.Errorf("mamori/vercel-gc: compacting value: %w: %w", mamori.ErrInvalid, err)
	}
	return buf.Bytes(), nil
}

// valueFor converts a stored value into a mamori.Value, applying #key selection
// when requested and hashing the resolved bytes for the version.
//
// Selection happens after unwrapping, matching valueFor in
// providers/launchdarkly, so selecting a field of a string-valued key fails
// with ErrInvalid rather than silently returning the whole string.
//
// Version is a content hash rather than the store digest: the digest is
// replaced whenever any key in the store changes, so using it would fire a
// spurious change for every unrelated field on every unrelated edit. The
// digest is reported in Metadata instead.
func valueFor(raw jsonRaw, ref mamori.Ref, storeID, digest string) (mamori.Value, error) {
	b, err := rawToBytes(raw)
	if err != nil {
		return mamori.Value{}, err
	}
	if ref.Key != "" {
		sel, err := mamori.SelectKey(b, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
		b = sel
	}
	return mamori.Value{
		Bytes:     b,
		Version:   mamori.VersionHash(b),
		Sensitive: false,
		Metadata: map[string]string{
			"store":  storeID,
			"digest": digest,
		},
	}, nil
}
