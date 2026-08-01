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

// TestRawToBytesErrorBranches pins rawToBytes's two error branches, neither
// of which was tested before this: the empty-after-trim case (only a key
// stored as the JSON literal null hits the "value exists" path; an actually
// empty or whitespace-only raw value is rejected instead) and the malformed-
// JSON-string case (a value starting with '"' whose content does not decode
// as a valid JSON string). Both are unreachable from Resolve today, because
// fetchItems's json.Unmarshal into map[string]jsonRaw already validates every
// stored value before rawToBytes ever sees it - confirmed by inspection, not
// asserted here - so this is defense in depth for any future or direct
// caller of rawToBytes, not a path Resolve exercises.
func TestRawToBytesErrorBranches(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: ""},
		{name: "whitespace only", raw: "   "},
		{name: "malformed string literal", raw: `"unterminated`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := rawToBytes(jsonRaw(tc.raw))
			if !errors.Is(err, mamori.ErrInvalid) {
				t.Fatalf("got %v, want an error satisfying mamori.ErrInvalid", err)
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

// TestValueForSelectingFieldOfStringDiscriminatesUnwrapOrder is
// TestValueForSelectingFieldOfStringIsInvalid's sharper sibling. That test's
// raw value is the JSON string "info", which fails selection whichever order
// unwrap-then-select happens in: selecting a field of the STRING "info"
// fails, and so does selecting a field of the raw JSON text `"info"` (quotes
// included), so the test proves only that selecting into a string fails, not
// that unwrapping happens first. Here the raw value is a JSON string whose
// DECODED content is itself a JSON object: selecting after unwrapping reaches
// that object and finds "timeout" inside it; selecting before unwrapping
// would instead try to select a field of the raw quoted text and fail. Only
// the unwrap-first order can produce "5s" here.
func TestValueForSelectingFieldOfStringDiscriminatesUnwrapOrder(t *testing.T) {
	ref := mamori.Ref{Scheme: scheme, Path: "api-config", Key: "timeout", Raw: "vercel-gc://api-config#timeout"}

	// The stored value is a JSON string whose own text is a JSON object.
	raw := jsonRaw(`"{\"timeout\":\"5s\"}"`)

	v, err := valueFor(raw, ref, "ecfg_abc", "dig1")
	if err != nil {
		t.Fatalf("unexpected error: %v (selection must unwrap the string first, then select #timeout from its decoded object content)", err)
	}
	if string(v.Bytes) != "5s" {
		t.Fatalf("got %q, want %q", v.Bytes, "5s")
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
