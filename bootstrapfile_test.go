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

// TestOpenRejectsASnapshotWithNoWriteTime pins that a snapshot which parses but
// carries no write time is refused rather than restored. Its age would read as
// zero forever, so BootstrapMaxAge could never expire it and a process would
// serve an arbitrarily old configuration while reporting a fresh one.
func TestOpenRejectsASnapshotWithNoWriteTime(t *testing.T) {
	s := sampleSnapshot()
	s.WrittenAt = time.Time{}
	sealed, err := sealSnapshot(s, testKey(t))
	if err != nil {
		t.Fatalf("sealSnapshot: %v", err)
	}
	if _, err := openSnapshot(sealed, testKey(t)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid for a snapshot with no write time", err)
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
