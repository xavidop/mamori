package mamori

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// schemaFingerprint is a stable hash of the shape a snapshot must satisfy.
//
// A snapshot restores resolved bytes into T's fields. If T has changed since
// the snapshot was written, replaying it fails somewhere downstream with a
// message about a value, when the real cause is that the config struct and the
// snapshot no longer describe the same thing. Comparing fingerprints turns that
// into one clear error naming schema drift.
//
// It is a correctness guard, not a security one: the snapshot is authenticated
// by AES-GCM, so this is not what stops tampering.
//
// The inputs are the parts that decide whether a stored value can still be
// restored into a field: the path it lands on, the refs it came from, its type,
// and whether it is sensitive. Order is deliberately excluded, because
// reordering a struct changes nothing about whether a snapshot fits it, and
// treating a cosmetic edit as drift would discard a usable snapshot.
func schemaFingerprint(specs []fieldSpec) string {
	lines := make([]string, 0, len(specs))
	for _, s := range specs {
		refs := make([]string, 0, len(s.Refs))
		for _, r := range s.Refs {
			refs = append(refs, r.String())
		}
		typeName := "<nil>"
		if s.Type != nil {
			typeName = s.Type.String()
		}
		lines = append(lines, strings.Join([]string{
			s.Path,
			strings.Join(refs, ","),
			typeName,
			boolField(s.Sensitive),
		}, "\x00"))
	}
	sort.Strings(lines)

	h := sha256.New()
	for _, l := range lines {
		h.Write([]byte(l))
		h.Write([]byte("\x01"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// boolField renders a bool for the fingerprint input.
func boolField(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
