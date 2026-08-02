package mamori

import (
	"bytes"
	"reflect"
	"sort"
	"strconv"

	"github.com/xavidop/mamori/secret"
)

// derivedVersionMaxDepth bounds the canonical walk. A config struct is a tree,
// but a pointer field can close a cycle (a parent link, a self-referential
// node), and the walk follows pointers by value rather than by address, so a
// cycle would otherwise recurse forever. Past this depth the walk emits a
// marker and stops descending: the version stays deterministic, it just stops
// distinguishing values that differ only below 32 levels of nesting.
const derivedVersionMaxDepth = 32

// derivedVersion returns a VersionHash of v's canonical bytes, giving a derived
// field the same kind of content-derived version a provider without a native
// revision already reports (builtin_exec.go, providers/aws/sm.go,
// providers/vault, and ~30 others all call VersionHash on the value's bytes).
// It returns "" for a value that cannot be read, matching the CanInterface
// guard report.go already applies before appending a Derived entry.
//
// The canonical bytes come from a recursive walk of v, not from formatting it,
// for two reasons a formatted %v gets wrong:
//
//   - secret.String and secret.Bytes redact themselves through String,
//     GoString, and MarshalJSON (secret.Redacted), so formatting one hashes the
//     constant "[REDACTED]": every secret derived field would report an
//     identical version, and that version would never change when the
//     underlying credential rotated - the precise failure WithDerive exists to
//     prevent, reintroduced one layer up. The walk reveals a secret wherever it
//     sits, not only at the top level: a write path naming a struct that holds
//     a secret.String field (which is exactly what the derived-fields guide
//     tells callers to assemble) hashes the credential inside it, and so does a
//     secret in a slice element, a map value, or behind a pointer.
//   - %v renders a pointer as its address, so a field holding *string would
//     report a fresh Version on every rebuild even when the pointee never
//     changed. The walk dereferences pointers and interfaces and hashes what
//     they point at, which is also what derivedFieldChanges compares with
//     reflect.DeepEqual: the reported version now churns exactly when
//     ev.Changed does.
//
// Two other identity-carrying cases are deliberately flattened: a chan, func,
// or unsafe.Pointer hashes as its kind name alone, since its address is
// process-local noise no operator can act on. Map entries are sorted by their
// encoded form, so Go's randomized map iteration order cannot change a version.
//
// The hash is not the value and is never a way back to it; a Report still
// carries no derived value at all (see TestReportJSONNeverCarriesDerivedValue).
func derivedVersion(v reflect.Value) string {
	if !v.IsValid() || !v.CanInterface() {
		return ""
	}
	return VersionHash(canonicalBytes(v))
}

// canonicalBytes returns the bytes derivedVersion hashes for v.
//
// A top-level secret, string, or byte slice hashes as exactly its own bytes,
// with no framing: that is what makes a derived string field's version
// byte-identical to the version a provider reports for the same content, so an
// operator comparing a derived row against a sourced one compares like with
// like (TestDerivedVersionMatchesVersionHash pins it). Every other shape is
// framed by appendCanonical, where the framing is what keeps two different
// values from colliding.
func canonicalBytes(v reflect.Value) []byte {
	switch v.Type() {
	case secretStringType:
		return v.Interface().(secret.String).RevealBytes()
	case secretBytesType:
		return v.Interface().(secret.Bytes).Reveal()
	}
	switch v.Kind() {
	case reflect.String:
		return []byte(v.String())
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 && !v.IsNil() {
			return appendElemBytes(nil, v)
		}
	}
	return appendCanonical(nil, v, 0)
}

// appendCanonical appends v's canonical encoding to dst. The encoding is
// self-delimiting (lengths for byte-ish values, terminators for composites) so
// that two different values cannot encode to the same bytes, and it never
// reads an address, a map iteration order, or anything else that varies
// between two runs over equal data.
//
// It reads unexported fields as well as exported ones, through reflect's
// read-only accessors rather than Interface, which is what lets it hash the
// content of a secret nested somewhere it cannot be revealed by type assertion
// (a secret.String held in an unexported field walks structurally into its
// unexported byte slice and hashes the same credential bytes).
func appendCanonical(dst []byte, v reflect.Value, depth int) []byte {
	if !v.IsValid() {
		return append(dst, "nil"...)
	}
	if depth > derivedVersionMaxDepth {
		return append(dst, "trunc"...)
	}
	if v.CanInterface() {
		switch v.Type() {
		case secretStringType:
			return appendLenBytes(append(dst, 's'), v.Interface().(secret.String).RevealBytes())
		case secretBytesType:
			return appendLenBytes(append(dst, 's'), v.Interface().(secret.Bytes).Reveal())
		}
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return append(dst, "nil"...)
		}
		return appendCanonical(append(dst, '*'), v.Elem(), depth+1)

	case reflect.Struct:
		t := v.Type()
		dst = append(dst, '{')
		for i := range v.NumField() {
			dst = append(dst, t.Field(i).Name...)
			dst = append(dst, '=')
			dst = appendCanonical(dst, v.Field(i), depth+1)
			dst = append(dst, ';')
		}
		return append(dst, '}')

	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.IsNil() {
			return append(dst, "nil"...)
		}
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return appendLenBytes(append(dst, 'b'), appendElemBytes(nil, v))
		}
		dst = append(dst, '[')
		for i := range v.Len() {
			dst = appendCanonical(dst, v.Index(i), depth+1)
			dst = append(dst, ';')
		}
		return append(dst, ']')

	case reflect.Map:
		if v.IsNil() {
			return append(dst, "nil"...)
		}
		entries := make([][]byte, 0, v.Len())
		for iter := v.MapRange(); iter.Next(); {
			e := appendCanonical(nil, iter.Key(), depth+1)
			e = append(e, '=')
			e = appendCanonical(e, iter.Value(), depth+1)
			entries = append(entries, e)
		}
		sort.Slice(entries, func(i, j int) bool { return bytes.Compare(entries[i], entries[j]) < 0 })
		dst = append(dst, 'm', '[')
		for _, e := range entries {
			dst = append(dst, e...)
			dst = append(dst, ';')
		}
		return append(dst, ']')

	case reflect.String:
		return appendLenBytes(append(dst, 's'), []byte(v.String()))

	case reflect.Bool:
		return strconv.AppendBool(dst, v.Bool())

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.AppendInt(dst, v.Int(), 10)

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.AppendUint(dst, v.Uint(), 10)

	case reflect.Float32, reflect.Float64:
		return strconv.AppendFloat(dst, v.Float(), 'g', -1, 64)

	case reflect.Complex64, reflect.Complex128:
		c := v.Complex()
		dst = strconv.AppendFloat(dst, real(c), 'g', -1, 64)
		dst = append(dst, '+')
		return strconv.AppendFloat(dst, imag(c), 'g', -1, 64)

	default:
		// Chan, Func, UnsafePointer: nothing but an address to read, and an
		// address is process-local noise that would churn the version on every
		// rebuild. Hash the kind so the shape still counts, the identity does
		// not.
		return append(dst, v.Kind().String()...)
	}
}

// appendLenBytes appends b length-prefixed, so "ab"+"c" cannot encode the same
// as "a"+"bc".
func appendLenBytes(dst []byte, b []byte) []byte {
	dst = strconv.AppendInt(dst, int64(len(b)), 10)
	dst = append(dst, ':')
	return append(dst, b...)
}

// appendElemBytes appends the elements of a byte slice or byte array one at a
// time. reflect.Value.Bytes would be shorter but is not available for a value
// obtained from an unexported field, which is exactly the case that matters
// here: a secret's own storage.
func appendElemBytes(dst []byte, v reflect.Value) []byte {
	for i := range v.Len() {
		dst = append(dst, byte(v.Index(i).Uint()))
	}
	return dst
}
