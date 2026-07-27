package mamori

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"testing"
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
	in := Value{
		Bytes:     []byte(base64.StdEncoding.EncodeToString([]byte("v"))),
		Version:   "abc123",
		Sensitive: true,
		Metadata:  map[string]string{"k": "v"},
	}
	got, err := applyDecode(ref, in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "abc123" {
		t.Errorf("Version = %q, want abc123 (the provider revision describes the source, not the decoded form)", got.Version)
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

func TestParseDecodePipelineUnknownCoding(t *testing.T) {
	_, err := parseDecodePipeline("base64,rot13")
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}
