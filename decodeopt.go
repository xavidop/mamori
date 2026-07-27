package mamori

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/hex"
	"errors"
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
//
// The whitespace handling here is deliberately asymmetric, and the asymmetry
// is load-bearing rather than an oversight: base64, base64url, and hex trim
// surrounding whitespace, gzip does not.
//
// The textual codings need the trim because stored values routinely pick up a
// trailing newline - `base64 < key.pem > secret` writes one, most editors add
// one on save, and several backends' CLIs round-trip through a file - and
// rejecting a secret over an invisible byte is a miserable failure to debug.
//
// gzip must NOT be trimmed, because its payload is binary: a valid gzip stream
// can legitimately END with bytes whose numeric values are ASCII whitespace.
// The 4-byte CRC32 and 4-byte ISIZE trailer are raw little-endian integers, so
// a trailer ending in 0x0a, 0x0d, 0x09, or 0x20 is entirely ordinary, and
// trimming it would silently corrupt a valid stream into a CRC or length
// mismatch. (The leading bytes are not at risk - a gzip stream always starts
// with the magic 0x1f 0x8b - but the trailing case alone settles it.)
//
// Please do not "fix" the inconsistency by trimming here. A genuinely
// whitespace-padded gzip payload already has an explicit escape hatch that
// does not put every other gzip value at risk: ?decode=trim,gzip.
var decodeCodings = map[string]func([]byte) ([]byte, error){
	"base64":    func(b []byte) ([]byte, error) { return base64.StdEncoding.DecodeString(string(bytes.TrimSpace(b))) },
	"base64url": func(b []byte) ([]byte, error) { return base64.URLEncoding.DecodeString(string(bytes.TrimSpace(b))) },
	"hex":       func(b []byte) ([]byte, error) { return hex.DecodeString(string(bytes.TrimSpace(b))) },
	"gzip":      gunzip,
	"trim":      func(b []byte) ([]byte, error) { return bytes.TrimSpace(b), nil },
}

// maxGzipDecoded bounds how many bytes one gzip coding will produce. gzip is
// the only coding here whose output is not linear in its input: base64 and hex
// shrink, trim cannot grow, but a few hundred bytes of gzip can expand to
// gigabytes. Since mamori resolves from remote backends whose contents an
// operator may not fully control - an S3 object, a Secrets Manager entry, a
// config server's response - an unbounded io.ReadAll here is a
// denial-of-service reachable by whoever can write the stored value.
//
// 16 MiB is chosen to sit far above every realistic payload and far below
// anything that threatens a process. The backends mamori reads from impose
// their own, much tighter ceilings on a single value: 64 KiB for AWS Secrets
// Manager and GCP Secret Manager, 25 KiB for Azure Key Vault, 512 KiB for
// Consul KV, 1 MiB for a Kubernetes Secret, ~1.5 MiB for an etcd value. The
// unbounded sources are the local ones (file:, exec:, dotenv:), and a 16 MiB
// decompressed application config or secret bundle from those is already well
// past anything this library is meant to carry. That leaves roughly a 16x
// margin over the most permissive remote backend while capping a bomb's
// expansion at a single, recoverable allocation.
//
// Exceeding it is an error, never a truncation. Handing an application 16 MiB
// of a longer secret would be worse than failing: a silently truncated key or
// certificate fails later, somewhere else, in a way that looks like anything
// but a decode problem.
//
// The cap is deliberately a constant with no Option to override it, so a
// legitimate payload above it - realistically only from an unbounded local
// source like file: or exec: - has no escape hatch short of not declaring
// ?decode=gzip and gunzipping in application code. That is the accepted cost
// of not growing the public API for a case no supported backend can even
// produce; raising the constant is the intended response if one ever appears.
const maxGzipDecoded = 16 << 20 // 16 MiB

func gunzip(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	// Read one byte past the cap so "expanded to exactly the cap" stays legal
	// and is still distinguishable from "expanded past it": io.LimitReader
	// alone reports a clean io.EOF at its limit, which io.ReadAll cannot tell
	// apart from a stream that genuinely ended there.
	out, err := io.ReadAll(io.LimitReader(zr, maxGzipDecoded+1))
	if err != nil {
		return nil, err
	}
	if len(out) > maxGzipDecoded {
		return nil, fmt.Errorf("payload expands past the %d-byte gzip decompression limit: %w", maxGzipDecoded, ErrInvalid)
	}
	return out, nil
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
			// A coding that already classified its own failure (gunzip's
			// decompression bound) is wrapped once; everything else - the
			// stdlib's own base64/hex errors, which carry no mamori sentinel -
			// gets the two-verb form so the classification is always present
			// exactly once in the chain rather than repeated in the message.
			if errors.Is(err, ErrInvalid) {
				return Value{}, fmt.Errorf("mamori: ref %q: %s decode failed: %w", redactRef(ref), s.name, err)
			}
			return Value{}, fmt.Errorf("mamori: ref %q: %s decode failed: %w: %w", redactRef(ref), s.name, ErrInvalid, err)
		}
	}
	v.Bytes = b
	return v, nil
}
