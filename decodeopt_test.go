package mamori

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestApplyDecodeRoundTrip(t *testing.T) {
	plain := []byte("s3cr3t")
	tests := []struct {
		spec string
		in   []byte
	}{
		{"base64", []byte(base64.StdEncoding.EncodeToString(plain))},
		{"base64url", []byte(base64.URLEncoding.EncodeToString(plain))},
		{"hex", []byte(hex.EncodeToString(plain))},
		{"gzip", gz(t, plain)},
		{"trim", []byte("  s3cr3t\n")},
	}
	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			ref, err := ParseRef("env:X?decode=" + tt.spec)
			if err != nil {
				t.Fatal(err)
			}
			got, err := applyDecode(ref, Value{Bytes: tt.in})
			if err != nil {
				t.Fatalf("applyDecode: %v", err)
			}
			if string(got.Bytes) != string(plain) {
				t.Errorf("= %q, want %q", got.Bytes, plain)
			}
		})
	}
}

func TestApplyDecodeChainAppliesLeftToRight(t *testing.T) {
	plain := []byte("s3cr3t")
	// decode=base64,gzip means: base64-decode first, then gunzip. So the stored
	// value is base64(gzip(plain)).
	stored := []byte(base64.StdEncoding.EncodeToString(gz(t, plain)))
	ref, err := ParseRef("env:X?decode=base64,gzip")
	if err != nil {
		t.Fatal(err)
	}
	got, err := applyDecode(ref, Value{Bytes: stored})
	if err != nil {
		t.Fatalf("applyDecode: %v", err)
	}
	if string(got.Bytes) != string(plain) {
		t.Errorf("= %q, want %q", got.Bytes, plain)
	}
}

func TestApplyDecodeFailureIsInvalid(t *testing.T) {
	for _, spec := range []string{"base64", "base64url", "hex", "gzip"} {
		t.Run(spec, func(t *testing.T) {
			ref, err := ParseRef("env:X?decode=" + spec)
			if err != nil {
				t.Fatal(err)
			}
			_, err = applyDecode(ref, Value{Bytes: []byte("!!! not valid !!!")})
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestApplyDecodePreservesMetadata(t *testing.T) {
	ref, err := ParseRef("env:X?decode=base64")
	if err != nil {
		t.Fatal(err)
	}
	notAfter := time.Date(2031, 4, 5, 6, 7, 8, 0, time.UTC)
	in := Value{
		Bytes:     []byte(base64.StdEncoding.EncodeToString([]byte("v"))),
		Version:   "abc123",
		Sensitive: true,
		NotAfter:  notAfter,
		Metadata:  map[string]string{"k": "v"},
	}
	got, err := applyDecode(ref, in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "abc123" {
		t.Errorf("Version = %q, want abc123 (the provider revision describes the source, not the decoded form)", got.Version)
	}
	if !got.NotAfter.Equal(notAfter) {
		t.Errorf("NotAfter = %v, want %v (expiry describes the source value and must survive decoding)", got.NotAfter, notAfter)
	}
	if !got.Sensitive {
		t.Error("Sensitive was lost across the decode pipeline")
	}
	if got.Metadata["k"] != "v" {
		t.Error("Metadata was lost across the decode pipeline")
	}
}

func TestApplyDecodeNoOptIsPassthrough(t *testing.T) {
	ref, err := ParseRef("env:X")
	if err != nil {
		t.Fatal(err)
	}
	got, err := applyDecode(ref, Value{Bytes: []byte("raw")})
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Bytes) != "raw" {
		t.Errorf("= %q, want raw", got.Bytes)
	}
}

// TestGunzipRejectsDecompressionBomb feeds gunzip a payload that is tiny on
// the wire and enormous once expanded. mamori reads from remote backends an
// operator may not fully control (an S3 object, a config server response), so
// an unbounded io.ReadAll here would let a crafted value exhaust the process's
// memory. The bound must be a loud error rather than a silent truncation: a
// truncated secret handed to the application is worse than a failed resolve.
func TestGunzipRejectsDecompressionBomb(t *testing.T) {
	bomb := gz(t, make([]byte, maxGzipDecoded+1)) // zeroes compress to a few hundred bytes
	if len(bomb) > 64*1024 {
		t.Fatalf("bomb is %d bytes on the wire; the test wants a genuinely small payload", len(bomb))
	}

	out, err := gunzip(bomb)
	if err == nil {
		t.Fatalf("gunzip accepted a %d-byte expansion (returned %d bytes); it must be bounded", maxGzipDecoded+1, len(out))
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want one wrapping ErrInvalid", err)
	}
	if out != nil {
		t.Errorf("gunzip returned %d bytes alongside the error; a truncated payload must never reach the caller", len(out))
	}

	// The same bomb through the public pipeline must fail the same way, so the
	// bound is real for every caller and not just a direct gunzip call.
	ref, err := ParseRef("env:X?decode=gzip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyDecode(ref, Value{Bytes: bomb}); !errors.Is(err, ErrInvalid) {
		t.Errorf("applyDecode err = %v, want one wrapping ErrInvalid", err)
	}
}

// TestGunzipAcceptsPayloadAtTheLimit pins that the bound is off-by-one-free:
// a payload expanding to exactly the cap is legal, only one byte more is not.
func TestGunzipAcceptsPayloadAtTheLimit(t *testing.T) {
	out, err := gunzip(gz(t, make([]byte, maxGzipDecoded)))
	if err != nil {
		t.Fatalf("gunzip rejected a payload of exactly the limit: %v", err)
	}
	if len(out) != maxGzipDecoded {
		t.Errorf("len = %d, want %d", len(out), maxGzipDecoded)
	}
}

func TestParseDecodePipelineUnknownCoding(t *testing.T) {
	_, err := parseDecodePipeline("base64,rot13")
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}
