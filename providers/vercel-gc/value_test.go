package vercelgc

import (
	"errors"
	"testing"

	"github.com/xavidop/mamori"
)

func TestRawToBytes(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "string is unquoted", raw: `"info"`, want: "info"},
		{name: "string with escapes is decoded", raw: `"a\"b\nc"`, want: "a\"b\nc"},
		{name: "true", raw: `true`, want: "true"},
		{name: "false", raw: `false`, want: "false"},
		{name: "integer", raw: `5432`, want: "5432"},
		{name: "float", raw: `0.25`, want: "0.25"},
		{name: "null is a value, not an absence", raw: `null`, want: "null"},
		{name: "object is compacted json", raw: "{\n  \"timeout\": \"5s\"\n}", want: `{"timeout":"5s"}`},
		{name: "array is compacted json", raw: `[1, 2, 3]`, want: `[1,2,3]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rawToBytes(jsonRaw(tc.raw))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValueForSelectsKey(t *testing.T) {
	ref := mamori.Ref{Scheme: scheme, Path: "api-config", Key: "timeout", Raw: "vercel-gc://api-config#timeout"}

	v, err := valueFor(jsonRaw(`{"timeout":"5s","retries":3}`), ref, "ecfg_abc", "dig1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(v.Bytes) != "5s" {
		t.Fatalf("got %q, want %q", v.Bytes, "5s")
	}
}

func TestValueForSelectsJSONPointer(t *testing.T) {
	ref := mamori.Ref{Scheme: scheme, Path: "api-config", Key: "/nested/deep", Raw: "vercel-gc://api-config#/nested/deep"}

	v, err := valueFor(jsonRaw(`{"nested":{"deep":"found"}}`), ref, "ecfg_abc", "dig1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(v.Bytes) != "found" {
		t.Fatalf("got %q, want %q", v.Bytes, "found")
	}
}

func TestValueForSelectingFieldOfStringIsInvalid(t *testing.T) {
	ref := mamori.Ref{Scheme: scheme, Path: "log-level", Key: "timeout", Raw: "vercel-gc://log-level#timeout"}

	_, err := valueFor(jsonRaw(`"info"`), ref, "ecfg_abc", "dig1")
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("got %v, want an error satisfying mamori.ErrInvalid", err)
	}
}

func TestValueForMetadataAndFlags(t *testing.T) {
	ref := mamori.Ref{Scheme: scheme, Path: "log-level", Raw: "vercel-gc://log-level"}

	v, err := valueFor(jsonRaw(`"info"`), ref, "ecfg_abc", "dig1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Sensitive {
		t.Error("Global Config holds flags, not managed secrets; Sensitive must be false")
	}
	if v.Version != mamori.VersionHash([]byte("info")) {
		t.Errorf("Version must hash the resolved bytes, got %q", v.Version)
	}
	if v.Metadata["store"] != "ecfg_abc" || v.Metadata["digest"] != "dig1" {
		t.Errorf("got metadata %v, want store=ecfg_abc digest=dig1", v.Metadata)
	}
}

func TestValueForVersionTracksSelectedBytes(t *testing.T) {
	ref := mamori.Ref{Scheme: scheme, Path: "api-config", Key: "timeout", Raw: "vercel-gc://api-config#timeout"}

	// Same selected field, different sibling. The version must not move: the
	// digest changes for any store edit, which is exactly why it is not the
	// version.
	a, err := valueFor(jsonRaw(`{"timeout":"5s","retries":3}`), ref, "ecfg_abc", "dig1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, err := valueFor(jsonRaw(`{"timeout":"5s","retries":9}`), ref, "ecfg_abc", "dig2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Version != b.Version {
		t.Fatalf("version moved on an unrelated edit: %q then %q", a.Version, b.Version)
	}
}
