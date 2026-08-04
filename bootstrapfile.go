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
//
// Ref is the field's primary ref, already redacted, and is recorded for
// diagnosis only: a replay rebuilds every fieldSpec from the running T, so
// nothing here is ever read back to decide where a value came from. That is
// also why a chain's winning position is not tracked - it would be information
// no reader consumes, stored beside live credentials.
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
	// A snapshot with no write time is refused rather than restored. Age is
	// computed from WrittenAt, and every reader of a zero one treats the age as
	// zero: the snapshot would look brand new forever, BootstrapMaxAge could
	// never expire it, and each restored field would report LastOK at the zero
	// time. The version check above does not cover this - a document sealed with
	// the right key and carrying version 1 but no written_at parses cleanly - so
	// it is checked here, at the one boundary both the restore and Doctor cross.
	if s.WrittenAt.IsZero() {
		return snapshot{}, fmt.Errorf("mamori: bootstrap snapshot carries no write time, so its age cannot be bounded: %w", ErrInvalid)
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
