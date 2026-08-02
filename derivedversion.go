package mamori

import (
	"fmt"
	"reflect"

	"github.com/xavidop/mamori/secret"
)

// derivedVersion returns a VersionHash of v's canonical bytes, giving a derived
// field the same kind of content-derived version a provider without a native
// revision already reports (builtin_exec.go, providers/aws/sm.go,
// providers/vault, and ~30 others all call VersionHash on the value's bytes).
// It returns "" for a value that cannot be read, matching the CanInterface
// guard report.go already applies before appending a Derived entry.
//
// secret.String and secret.Bytes are revealed rather than formatted. Both
// redact themselves through String, GoString, and MarshalJSON (secret.Redacted),
// so formatting one would hash the constant "[REDACTED]": every secret derived
// field would report an identical version, and that version would never change
// when the underlying credential rotated - the precise failure WithDerive
// exists to prevent, reintroduced one layer up. The hash is not the value and
// is never a way back to it; a Report still carries no derived value at all
// (see TestReportJSONNeverCarriesDerivedValue).
func derivedVersion(v reflect.Value) string {
	if !v.IsValid() || !v.CanInterface() {
		return ""
	}
	switch v.Type() {
	case secretStringType:
		return VersionHash(v.Interface().(secret.String).RevealBytes())
	case secretBytesType:
		return VersionHash(v.Interface().(secret.Bytes).Reveal())
	}
	return VersionHash(fmt.Appendf(nil, "%v", v.Interface()))
}
