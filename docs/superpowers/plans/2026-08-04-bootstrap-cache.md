# Bootstrap cache Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** a process that restarts while its configuration backend is unreachable boots from an encrypted on-disk snapshot of the last known-good resolved values, instead of failing to start.

**Architecture:** the snapshot stores **resolved values, not the decoded struct**, because `secret.String.MarshalJSON` returns the redaction by design and serialising `T` would silently lose every secret. On a cold start the ordinary resolve runs first; only if it fails with a *transient* kind is the snapshot read and replayed through the normal decode, derive and validate path. Six new files in the root module, plus wiring into `Watch`, `Report`, `Health` and `Doctor`.

**Tech Stack:** Go 1.26, standard library only (`crypto/aes`, `crypto/cipher`, `crypto/rand`, `encoding/json`, `os`). No new dependency.

**Spec:** `docs/superpowers/specs/2026-08-04-bootstrap-cache-design.md`.

## Global Constraints

- Go 1.26. The root module's dependencies are deliberately minimal; this feature adds **no** new one.
- Errors with an underlying cause wrap with TWO `%w` verbs: `fmt.Errorf("%w: %w", sentinel, err)`. A single `%w` with `%v` flattens the chain and breaks `errors.As`. See `errors.go:148-156`.
- **A snapshot never reaches a log, an error message, a `Report`, or the admin endpoint.** It holds live credentials. Errors name the field path and the failure, never bytes.
- The snapshot file is mode `0600`, written to a temporary file in the same directory and renamed over the target.
- Do NOT use the em-dash character anywhere, Go or markdown. Hard project rule.
- Every exported identifier carries a doc comment explaining WHY, not just what.
- Gates: `make test`, `make lint`, and `go test -race ./...` in the root module.

## File Structure

| File | Responsibility |
| --- | --- |
| `bootstrap.go` | `WithBootstrapCache`, `BootstrapMaxAge`, option validation |
| `bootstrapfile.go` | the on-disk format: `snapshot`, `seal`, `open`, atomic write, `0600` |
| `bootstrapfingerprint.go` | the schema fingerprint over `[]fieldSpec` |
| `bootstrap_test.go`, `bootstrapfile_test.go`, `bootstrapfingerprint_test.go` | tests |
| `reconciler.go` | wire the cold-start fallback and the write-on-apply |
| `status.go`, `report.go` | `Report` gains the source and age; `Health` gains the maxAge rule |

---

### Task 1: the schema fingerprint

First because it is a pure function over an existing type, so it establishes the test loop with no I/O and no crypto.

**Files:**
- Create: `bootstrapfingerprint.go`
- Test: `bootstrapfingerprint_test.go`

**Interfaces:**
- Consumes: `fieldSpec` (`resolve.go`), `Ref`
- Produces: `func schemaFingerprint(specs []fieldSpec) string`

- [ ] **Step 1: Write the failing test**

Create `bootstrapfingerprint_test.go`:

```go
package mamori

import (
	"reflect"
	"testing"
)

// specFor builds a minimal fieldSpec for fingerprint tests.
func specFor(path, ref string, sensitive bool) fieldSpec {
	r, err := ParseRef(ref)
	if err != nil {
		panic("bad ref in test: " + ref)
	}
	return fieldSpec{
		Path:      path,
		Refs:      []Ref{r},
		Type:      reflect.TypeOf(""),
		Sensitive: sensitive,
	}
}

func TestSchemaFingerprintIsStable(t *testing.T) {
	a := []fieldSpec{specFor("A", "env:A", false), specFor("B", "env:B", true)}
	b := []fieldSpec{specFor("A", "env:A", false), specFor("B", "env:B", true)}
	if schemaFingerprint(a) != schemaFingerprint(b) {
		t.Fatal("identical specs produced different fingerprints")
	}
}

// TestSchemaFingerprintIgnoresSpecOrder pins that a reordered struct is not
// treated as a different schema. Field order carries no meaning for whether a
// snapshot can satisfy a config, and treating it as drift would throw away a
// usable snapshot after a cosmetic edit.
func TestSchemaFingerprintIgnoresSpecOrder(t *testing.T) {
	a := []fieldSpec{specFor("A", "env:A", false), specFor("B", "env:B", false)}
	b := []fieldSpec{specFor("B", "env:B", false), specFor("A", "env:A", false)}
	if schemaFingerprint(a) != schemaFingerprint(b) {
		t.Fatal("reordering the specs changed the fingerprint")
	}
}

func TestSchemaFingerprintChangesOn(t *testing.T) {
	base := []fieldSpec{specFor("A", "env:A", false)}
	tests := []struct {
		name  string
		specs []fieldSpec
	}{
		{"an added field", []fieldSpec{specFor("A", "env:A", false), specFor("B", "env:B", false)}},
		{"a removed field", nil},
		{"a renamed field", []fieldSpec{specFor("Z", "env:A", false)}},
		{"a changed ref", []fieldSpec{specFor("A", "env:CHANGED", false)}},
		{"a changed sensitivity", []fieldSpec{specFor("A", "env:A", true)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if schemaFingerprint(base) == schemaFingerprint(tt.specs) {
				t.Fatalf("%s did not change the fingerprint", tt.name)
			}
		})
	}
}

// TestSchemaFingerprintChangesOnType pins that a field keeping its name and ref
// but changing type is drift. Restoring bytes into the wrong type would fail
// decoding with a message about the value rather than about the schema.
func TestSchemaFingerprintChangesOnType(t *testing.T) {
	a := []fieldSpec{{Path: "A", Type: reflect.TypeOf("")}}
	b := []fieldSpec{{Path: "A", Type: reflect.TypeOf(0)}}
	if schemaFingerprint(a) == schemaFingerprint(b) {
		t.Fatal("changing a field's type did not change the fingerprint")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -run TestSchemaFingerprint ./... 2>&1 | head -20`
Expected: FAIL, `undefined: schemaFingerprint`

- [ ] **Step 3: Implement it**

Create `bootstrapfingerprint.go`:

```go
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -run TestSchemaFingerprint ./...`
Expected: PASS

- [ ] **Step 5: Prove each test can fail**

For each of `TestSchemaFingerprintIgnoresSpecOrder` and `TestSchemaFingerprintChangesOnType`, break the behaviour it covers (remove the `sort.Strings`; drop `typeName` from the joined line), confirm that specific test FAILS, then revert. Report both states.

- [ ] **Step 6: Commit**

```bash
git add bootstrapfingerprint.go bootstrapfingerprint_test.go
git commit -m "feat(core): a schema fingerprint for bootstrap snapshots"
```

---

### Task 2: the on-disk format

**Files:**
- Create: `bootstrapfile.go`
- Test: `bootstrapfile_test.go`

**Interfaces:**
- Consumes: `Value`, `schemaFingerprint` (Task 1)
- Produces:
  - `type snapshot struct { Version int; WrittenAt time.Time; Fingerprint string; Records []snapshotRecord }`
  - `type snapshotRecord struct { Path, Ref, ValueVersion string; Bytes []byte; Sensitive bool; NotAfter time.Time; Metadata map[string]string }`
  - `func sealSnapshot(s snapshot, key []byte) ([]byte, error)`
  - `func openSnapshot(b, key []byte) (snapshot, error)`
  - `func writeSnapshotFile(path string, b []byte) error`
  - `const snapshotFormatVersion = 1`

- [ ] **Step 1: Write the failing test**

Create `bootstrapfile_test.go`:

```go
package mamori

import (
	"bytes"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testKey returns a deterministic 32-byte key.
func testKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

// sampleSnapshot returns a snapshot carrying one sensitive record.
func sampleSnapshot() snapshot {
	return snapshot{
		Version:     snapshotFormatVersion,
		WrittenAt:   time.Unix(1_700_000_000, 0).UTC(),
		Fingerprint: "fp",
		Records: []snapshotRecord{{
			Path:         "DB.Password",
			Ref:          "env:DB_PASSWORD",
			Bytes:        []byte("s3cr3t"),
			ValueVersion: "v1",
			Sensitive:    true,
		}},
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	key := testKey(t)
	sealed, err := sealSnapshot(sampleSnapshot(), key)
	if err != nil {
		t.Fatalf("sealSnapshot: %v", err)
	}
	got, err := openSnapshot(sealed, key)
	if err != nil {
		t.Fatalf("openSnapshot: %v", err)
	}
	if len(got.Records) != 1 || string(got.Records[0].Bytes) != "s3cr3t" {
		t.Fatalf("round trip lost the value: %+v", got.Records)
	}
	if !got.Records[0].Sensitive {
		t.Fatal("round trip lost the sensitive flag")
	}
}

// TestSealedBytesDoNotContainThePlaintext is the whole point of sealing: the
// file must not carry the secret in the clear.
func TestSealedBytesDoNotContainThePlaintext(t *testing.T) {
	sealed, err := sealSnapshot(sampleSnapshot(), testKey(t))
	if err != nil {
		t.Fatalf("sealSnapshot: %v", err)
	}
	if bytes.Contains(sealed, []byte("s3cr3t")) {
		t.Fatal("the sealed bytes contain the plaintext secret")
	}
	if bytes.Contains(sealed, []byte("DB.Password")) {
		t.Fatal("the sealed bytes contain a field path in the clear")
	}
}

func TestSealUsesAFreshNonce(t *testing.T) {
	key := testKey(t)
	a, err := sealSnapshot(sampleSnapshot(), key)
	if err != nil {
		t.Fatalf("sealSnapshot: %v", err)
	}
	b, err := sealSnapshot(sampleSnapshot(), key)
	if err != nil {
		t.Fatalf("sealSnapshot: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("two seals of identical input are byte-identical; the nonce is not fresh")
	}
}

func TestOpenRejectsAWrongKey(t *testing.T) {
	sealed, err := sealSnapshot(sampleSnapshot(), testKey(t))
	if err != nil {
		t.Fatalf("sealSnapshot: %v", err)
	}
	wrong := make([]byte, 32)
	if _, err := rand.Read(wrong); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if _, err := openSnapshot(sealed, wrong); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

// TestOpenRejectsTamperedCiphertext pins that AES-GCM's authentication is
// actually checked, which is what stops an edited snapshot from being restored.
func TestOpenRejectsTamperedCiphertext(t *testing.T) {
	key := testKey(t)
	sealed, err := sealSnapshot(sampleSnapshot(), key)
	if err != nil {
		t.Fatalf("sealSnapshot: %v", err)
	}
	sealed[len(sealed)-1] ^= 0xff
	if _, err := openSnapshot(sealed, key); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid for tampered ciphertext", err)
	}
}

func TestOpenRejectsShortInput(t *testing.T) {
	if _, err := openSnapshot([]byte("tiny"), testKey(t)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestOpenRejectsAFutureFormatVersion(t *testing.T) {
	s := sampleSnapshot()
	s.Version = snapshotFormatVersion + 1
	sealed, err := sealSnapshot(s, testKey(t))
	if err != nil {
		t.Fatalf("sealSnapshot: %v", err)
	}
	if _, err := openSnapshot(sealed, testKey(t)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid for a newer format", err)
	}
}

func TestWriteSnapshotFileIsAtomicAndPrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snap")

	if err := writeSnapshotFile(path, []byte("first")); err != nil {
		t.Fatalf("writeSnapshotFile: %v", err)
	}
	if err := writeSnapshotFile(path, []byte("second")); err != nil {
		t.Fatalf("writeSnapshotFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "second" {
		t.Fatalf("content = %q, want second", got)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %o, want 600; a snapshot holds live credentials", perm)
	}

	// No temporary file may survive a successful write.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory holds %v, want only the snapshot", names)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -run 'TestSeal|TestOpen|TestWriteSnapshot' ./... 2>&1 | head -20`
Expected: FAIL, `undefined: sealSnapshot`

- [ ] **Step 3: Implement it**

Create `bootstrapfile.go`:

```go
package mamori

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// snapshotFormatVersion is the on-disk format version. openSnapshot refuses a
// snapshot written by a newer mamori rather than guessing at fields it does not
// know, because the alternative is restoring a partially understood config.
const snapshotFormatVersion = 1

// snapshot is the plaintext of a bootstrap cache file.
//
// It holds resolved values rather than the decoded T, because secret.String
// marshals to its redaction by design: serialising T would faithfully persist
// "[REDACTED]" and silently lose every secret.
type snapshot struct {
	Version     int              `json:"version"`
	WrittenAt   time.Time        `json:"written_at"`
	Fingerprint string           `json:"fingerprint"`
	Records     []snapshotRecord `json:"records"`
}

// snapshotRecord is one field's resolved value, enough to replay it through the
// ordinary decode and validate path without contacting a provider.
type snapshotRecord struct {
	Path         string            `json:"path"`
	Ref          string            `json:"ref"`
	Bytes        []byte            `json:"bytes"`
	ValueVersion string            `json:"value_version"`
	Sensitive    bool              `json:"sensitive"`
	NotAfter     time.Time         `json:"not_after,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

// sealSnapshot encrypts s with AES-256-GCM under key, returning nonce||ciphertext.
//
// The whole document is sealed, not only the values: a field path and a ref are
// not secrets in themselves, but together they map an attacker's view of what
// this process reads and from where, and there is no reason to hand that over
// when the payload is already encrypted.
func sealSnapshot(s snapshot, key []byte) ([]byte, error) {
	plain, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("mamori: encoding bootstrap snapshot: %w: %w", ErrInvalid, err)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("mamori: bootstrap snapshot nonce: %w: %w", ErrUnavailable, err)
	}
	return gcm.Seal(nonce, nonce, plain, nil), nil
}

// openSnapshot decrypts and parses a sealed snapshot.
//
// Every failure wraps ErrInvalid rather than ErrNotFound. A snapshot that will
// not open is a broken fallback the operator must know about, and ErrNotFound is
// the one kind mamori treats as "apply the default", which would hide it.
func openSnapshot(b, key []byte) (snapshot, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return snapshot{}, err
	}
	if len(b) < gcm.NonceSize() {
		return snapshot{}, fmt.Errorf("mamori: bootstrap snapshot is truncated: %w", ErrInvalid)
	}
	nonce, ct := b[:gcm.NonceSize()], b[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		// Deliberately not wrapping err: it is "cipher: message authentication
		// failed" for both a wrong key and a tampered file, and repeating it
		// adds nothing an operator can act on.
		return snapshot{}, fmt.Errorf("mamori: bootstrap snapshot did not authenticate, the key may be wrong or the file altered: %w", ErrInvalid)
	}
	var s snapshot
	if err := json.Unmarshal(plain, &s); err != nil {
		return snapshot{}, fmt.Errorf("mamori: bootstrap snapshot is not valid JSON: %w: %w", ErrInvalid, err)
	}
	if s.Version != snapshotFormatVersion {
		return snapshot{}, fmt.Errorf("mamori: bootstrap snapshot format version %d, this build understands %d: %w",
			s.Version, snapshotFormatVersion, ErrInvalid)
	}
	return s, nil
}

// newGCM builds the AEAD, rejecting a key that is not 32 bytes so the caller
// learns at construction rather than at the first write.
func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("mamori: bootstrap cache key is %d bytes, want 32 for AES-256: %w", len(key), ErrInvalid)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("mamori: bootstrap cache cipher: %w: %w", ErrInvalid, err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("mamori: bootstrap cache AEAD: %w: %w", ErrInvalid, err)
	}
	return gcm, nil
}

// writeSnapshotFile writes b to path atomically, mode 0600.
//
// The temporary file is created in the same directory so the rename stays within
// one filesystem, and it is removed on any failure. A crash mid-write therefore
// leaves the previous snapshot intact rather than a truncated one, which matters
// because a truncated snapshot is exactly the fallback that would be needed next.
func writeSnapshotFile(path string, b []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".mamori-snapshot-*")
	if err != nil {
		return fmt.Errorf("mamori: creating bootstrap snapshot: %w: %w", ErrUnavailable, err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // no-op once the rename succeeds

	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("mamori: securing bootstrap snapshot: %w: %w", ErrUnavailable, err)
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return fmt.Errorf("mamori: writing bootstrap snapshot: %w: %w", ErrUnavailable, err)
	}
	// Sync before rename: without it a crash can leave a renamed but empty file,
	// which is worse than no snapshot because it looks usable.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("mamori: syncing bootstrap snapshot: %w: %w", ErrUnavailable, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("mamori: closing bootstrap snapshot: %w: %w", ErrUnavailable, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("mamori: replacing bootstrap snapshot: %w: %w", ErrUnavailable, err)
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -run 'TestSeal|TestOpen|TestWriteSnapshot' -v ./... 2>&1 | grep -E '^(--- |ok|FAIL)'`
Expected: all PASS

- [ ] **Step 5: Prove the security tests can fail**

Three mutations, each reverted after:
- Make `sealSnapshot` return the plaintext JSON unencrypted. `TestSealedBytesDoNotContainThePlaintext` must FAIL.
- Make the nonce a fixed zero array. `TestSealUsesAFreshNonce` must FAIL.
- Change `writeSnapshotFile` to `os.WriteFile(path, b, 0o644)`. `TestWriteSnapshotFileIsAtomicAndPrivate` must FAIL on the mode assertion.

Report both states for each. A security test that cannot fail is worse than none.

- [ ] **Step 6: Commit**

```bash
git add bootstrapfile.go bootstrapfile_test.go
git commit -m "feat(core): the encrypted bootstrap snapshot file format"
```

---

### Task 3: the option

**Files:**
- Create: `bootstrap.go`
- Modify: `reconcile.go` (the `options` struct only)
- Test: `bootstrap_test.go`

**Interfaces:**
- Consumes: `newGCM` (Task 2)
- Produces:
  - `func WithBootstrapCache(path string, key []byte, opts ...BootstrapOption) Option`
  - `type BootstrapOption func(*bootstrapConfig)`
  - `func BootstrapMaxAge(d time.Duration) BootstrapOption`
  - `const DefaultBootstrapMaxAge = 24 * time.Hour`
  - `options` gains a `bootstrap *bootstrapConfig` field

- [ ] **Step 1: Write the failing test**

Create `bootstrap_test.go`:

```go
package mamori

import (
	"errors"
	"testing"
	"time"
)

// applyOpts runs opts against a fresh options value, as Watch does.
func applyOpts(t *testing.T, opts ...Option) *options {
	t.Helper()
	o := &options{}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

func TestWithBootstrapCacheDefaults(t *testing.T) {
	o := applyOpts(t, WithBootstrapCache("/tmp/snap", testKey(t)))
	if o.bootstrap == nil {
		t.Fatal("bootstrap config not set")
	}
	if o.bootstrap.maxAge != DefaultBootstrapMaxAge {
		t.Fatalf("maxAge = %v, want %v", o.bootstrap.maxAge, DefaultBootstrapMaxAge)
	}
	if o.bootstrap.err != nil {
		t.Fatalf("unexpected error: %v", o.bootstrap.err)
	}
}

func TestBootstrapMaxAgeOverrides(t *testing.T) {
	o := applyOpts(t, WithBootstrapCache("/tmp/snap", testKey(t), BootstrapMaxAge(2*time.Hour)))
	if o.bootstrap.maxAge != 2*time.Hour {
		t.Fatalf("maxAge = %v, want 2h", o.bootstrap.maxAge)
	}
}

// TestWithBootstrapCacheRejectsABadKey pins that a wrong-sized key fails at
// construction. Deferring it to the first write means the process learns its
// fallback was never viable only once it needs it.
func TestWithBootstrapCacheRejectsABadKey(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33} {
		o := applyOpts(t, WithBootstrapCache("/tmp/snap", make([]byte, n)))
		if o.bootstrap == nil || o.bootstrap.err == nil {
			t.Fatalf("a %d-byte key was accepted", n)
		}
		if !errors.Is(o.bootstrap.err, ErrInvalid) {
			t.Fatalf("err = %v, want ErrInvalid", o.bootstrap.err)
		}
	}
}

func TestWithBootstrapCacheRejectsAnEmptyPath(t *testing.T) {
	o := applyOpts(t, WithBootstrapCache("", testKey(t)))
	if o.bootstrap == nil || !errors.Is(o.bootstrap.err, ErrInvalid) {
		t.Fatalf("an empty path was accepted: %+v", o.bootstrap)
	}
}

// TestBootstrapMaxAgeZeroIsUnbounded pins that zero is an explicit opt-out
// rather than an accident, since the option must be written to reach it.
func TestBootstrapMaxAgeZeroIsUnbounded(t *testing.T) {
	o := applyOpts(t, WithBootstrapCache("/tmp/snap", testKey(t), BootstrapMaxAge(0)))
	if o.bootstrap.maxAge != 0 {
		t.Fatalf("maxAge = %v, want 0", o.bootstrap.maxAge)
	}
}

// TestBootstrapKeyIsCopied pins that mutating the caller's key slice after the
// option is built cannot change what the snapshot is sealed with.
func TestBootstrapKeyIsCopied(t *testing.T) {
	key := testKey(t)
	o := applyOpts(t, WithBootstrapCache("/tmp/snap", key))
	before := append([]byte(nil), o.bootstrap.key...)
	for i := range key {
		key[i] = 0xff
	}
	for i := range before {
		if o.bootstrap.key[i] != before[i] {
			t.Fatal("mutating the caller's slice changed the stored key")
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -run 'TestWithBootstrapCache|TestBootstrap' ./... 2>&1 | head -20`
Expected: FAIL, `undefined: WithBootstrapCache`

- [ ] **Step 3: Add the options field**

In `reconcile.go`, add to the `options` struct, beside `stale`:

```go
	bootstrap *bootstrapConfig // nil = disabled
```

- [ ] **Step 4: Implement the option**

Create `bootstrap.go`:

```go
package mamori

import (
	"fmt"
	"time"
)

// DefaultBootstrapMaxAge bounds how old a restored snapshot may be while
// Health still reports the process ready. See BootstrapMaxAge.
const DefaultBootstrapMaxAge = 24 * time.Hour

// bootstrapConfig is the resolved WithBootstrapCache configuration.
//
// err carries a construction failure rather than panicking, because an Option
// cannot return one. Watch surfaces it before resolving anything.
type bootstrapConfig struct {
	path   string
	key    []byte
	maxAge time.Duration
	err    error
}

// BootstrapOption configures the bootstrap cache.
type BootstrapOption func(*bootstrapConfig)

// BootstrapMaxAge bounds how old a restored snapshot may be while Health
// reports the process ready. It defaults to DefaultBootstrapMaxAge.
//
// Inside the bound, a process that booted from a snapshot passes Health so it
// joins the load balancer, which is the point: a backend outage should not also
// be a total outage. Past it, Health fails, so a config frozen for longer than
// the operator tolerates takes the process out rather than serving indefinitely
// stale secrets.
//
// Set this to the rotation window of the shortest-lived credential in the
// config. A process serving credentials older than that will fail against the
// backend that rotated them, and failing readiness is the better outcome.
//
// Zero means unbounded. It has to be written explicitly, because a config that
// is stale forever and silent about it is not a default anyone should get by
// accident.
func BootstrapMaxAge(d time.Duration) BootstrapOption {
	return func(c *bootstrapConfig) { c.maxAge = d }
}

// WithBootstrapCache keeps an encrypted snapshot of the last known-good resolved
// values at path, and boots from it when a cold start cannot reach the backend.
//
// It is a fallback, never a fast path. Every start resolves normally first, and
// the snapshot is read only when that fails with a transient kind
// (KindUnavailable or KindRateLimited). A backend that answers and says no, a
// deleted secret or a revoked credential, fails the start: serving a cached copy
// of a value the backend deliberately removed would defeat the revocation.
//
// key must be 32 bytes; the snapshot is sealed with AES-256-GCM and the file is
// written atomically with mode 0600. Where the key comes from is a deployment
// concern, because the whole point is that mamori's own backends are unreachable
// at the moment this is needed.
//
// Enabling this creates an artifact holding live credentials at rest that did
// not exist before. That is the trade: a startup failure for a file an attacker
// with disk access and the key could read. Decide it deliberately.
func WithBootstrapCache(path string, key []byte, opts ...BootstrapOption) Option {
	c := &bootstrapConfig{path: path, maxAge: DefaultBootstrapMaxAge}
	// Copied, so a caller reusing or zeroing its buffer after this call cannot
	// change what later snapshots are sealed with.
	c.key = append([]byte(nil), key...)
	for _, opt := range opts {
		opt(c)
	}
	switch {
	case c.path == "":
		c.err = fmt.Errorf("mamori: WithBootstrapCache path is required: %w", ErrInvalid)
	default:
		if _, err := newGCM(c.key); err != nil {
			c.err = err
		}
	}
	return func(o *options) { o.bootstrap = c }
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -run 'TestWithBootstrapCache|TestBootstrap' ./...`
Expected: PASS

- [ ] **Step 6: Prove the key copy is real**

Remove the `append([]byte(nil), key...)` copy so the slice is stored directly, confirm `TestBootstrapKeyIsCopied` FAILS, then revert. Report both states.

- [ ] **Step 7: Commit**

```bash
git add bootstrap.go bootstrap_test.go reconcile.go
git commit -m "feat(core): WithBootstrapCache option and its validation"
```

---

### Tasks 4 to 7

Task 3 completes the standalone pieces. The remaining work wires them into the reconciler and the reporting surfaces, and each of those tasks needs its interface fixed by the task before it, so their steps are written after Task 3 lands rather than guessed now.

**Task 4: write the snapshot on every applied update.** Hook the point where a candidate has passed validation, `PreApply` and `WithDerive`, and persist the resolved values behind it. A write failure must never fail the update: the config is good and refusing it because a cache file could not be written would invert the feature's purpose. Report the failure through `OnError` and a meter counter instead.

**Task 5: the cold-start fallback.** In `Watch` and `Load`, when `loadValue` fails, classify the failure; on `KindUnavailable` or `KindRateLimited` read the snapshot, check the fingerprint, refuse any record whose `NotAfter` has passed, and replay the records through the ordinary decode, derive and validate path. Any failure here wraps both the original resolve error and the cache error.

**Task 6: reporting.** `Report` gains the snapshot source and age; `Health` fails once the age exceeds `maxAge`; `Doctor` reports whether a snapshot exists, its age and its fingerprint match.

**Task 7: documentation.** `site/src/pages/docs/usage/bootstrap-cache.md` plus a sidebar entry, the root `README.md` feature bullet, `site/src/pages/docs/usage/options.md`, `site/src/pages/docs/observability/index.md`, and `skills/mamori/SKILL.md`. The security trade stated in the spec must appear in the docs, not only in the doc comment.

## Self-Review Notes

**Spec coverage.** Tasks 1 to 3 cover the fingerprint, the file format and the option, which are every piece the spec describes that can be built without touching the reconciler. Tasks 4 to 7 are scoped above and their steps are written once Task 3 fixes the interfaces they consume.

**Placeholder scan.** Tasks 1 to 3 contain complete code and no placeholders. Tasks 4 to 7 are deliberately scoped rather than detailed, which is a decomposition boundary and is stated as one.

**Type consistency.** `snapshot`, `snapshotRecord`, `sealSnapshot`, `openSnapshot`, `writeSnapshotFile` and `newGCM` are defined in Task 2 and consumed in Task 3 (`newGCM`) with matching signatures. `schemaFingerprint` is defined in Task 1 and consumed by the snapshot's `Fingerprint` field. `testKey` is defined in Task 2's test file and reused by Task 3's, both in package `mamori`.
