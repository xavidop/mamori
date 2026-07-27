package mamori

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

// decodeStep is one coding in a ?decode= pipeline, carrying its name so a
// failure can say which stage failed rather than only that decoding failed.
type decodeStep struct {
	name string
	fn   func([]byte) ([]byte, error)
}

// decodeCodings is the closed set of codings ?decode= understands. It is
// deliberately closed and deliberately stdlib-only: core's dependency set is a
// stated property of the project layout, and an extension point here would
// duplicate WithDecodeHook, which already exists one layer down for arbitrary
// per-type conversion.
var decodeCodings = map[string]func([]byte) ([]byte, error){
	"base64":    func(b []byte) ([]byte, error) { return base64.StdEncoding.DecodeString(string(bytes.TrimSpace(b))) },
	"base64url": func(b []byte) ([]byte, error) { return base64.URLEncoding.DecodeString(string(bytes.TrimSpace(b))) },
	"hex":       func(b []byte) ([]byte, error) { return hex.DecodeString(string(bytes.TrimSpace(b))) },
	"gzip":      gunzip,
	"trim":      func(b []byte) ([]byte, error) { return bytes.TrimSpace(b), nil },
}

func gunzip(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	return io.ReadAll(zr)
}

// parseDecodePipeline turns a ?decode= option value into an ordered pipeline.
// Codings are applied left to right, outermost wrapper first: "base64,gzip"
// means the stored value is base64 of gzip of the payload, so it is
// base64-decoded and then gunzipped.
//
// The option is named decode rather than encoding precisely so that order is
// unambiguous. HTTP's Content-Encoding lists codings in the order they were
// applied and is therefore decoded in reverse; naming this one after the action
// removes any question of which direction the list reads.
func parseDecodePipeline(spec string) ([]decodeStep, error) {
	if spec == "" {
		return nil, nil
	}
	parts := strings.Split(spec, ",")
	steps := make([]decodeStep, 0, len(parts))
	for _, p := range parts {
		name := strings.TrimSpace(p)
		fn, ok := decodeCodings[name]
		if !ok {
			return nil, fmt.Errorf("mamori: unknown decode coding %q (want base64, base64url, hex, gzip, or trim): %w", name, ErrInvalid)
		}
		steps = append(steps, decodeStep{name: name, fn: fn})
	}
	return steps, nil
}

// applyDecode runs ref's ?decode= pipeline over v's bytes, returning v
// unchanged when the ref carries no decode option.
//
// Version, Sensitive, NotAfter, and Metadata are carried through untouched. In
// particular Version still describes the provider's revision of the source, not
// the decoded form, so Value.changed keeps detecting change exactly as it does
// for an undecoded field.
//
// The pipeline is re-derived per call rather than cached on the Ref: it is a
// string split and a map lookup per coding, which is free next to the network
// round trip that produced v, and caching it would mean giving Ref mutable
// state it does not otherwise have.
func applyDecode(ref Ref, v Value) (Value, error) {
	steps, err := parseDecodePipeline(ref.Opt("decode"))
	if err != nil {
		return Value{}, fmt.Errorf("mamori: ref %q: %w", redactRef(ref), err)
	}
	if len(steps) == 0 {
		return v, nil
	}
	b := v.Bytes
	for _, s := range steps {
		b, err = s.fn(b)
		if err != nil {
			return Value{}, fmt.Errorf("mamori: ref %q: %s decode failed: %w: %w", redactRef(ref), s.name, ErrInvalid, err)
		}
	}
	v.Bytes = b
	return v, nil
}
