package httpcore

import (
	"testing"

	"github.com/xavidop/mamori"
)

func TestVersionPrefersETag(t *testing.T) {
	got := Version(&Response{ETag: `W/"abc"`, LastModified: "Wed, 21 Oct 2026 07:28:00 GMT"}, []byte("x"))
	if got != `W/"abc"` {
		t.Fatalf("Version = %q, want the ETag", got)
	}
}

func TestVersionFallsBackToLastModified(t *testing.T) {
	got := Version(&Response{LastModified: "Wed, 21 Oct 2026 07:28:00 GMT"}, []byte("x"))
	if got != "Wed, 21 Oct 2026 07:28:00 GMT" {
		t.Fatalf("Version = %q, want the Last-Modified", got)
	}
}

func TestVersionFallsBackToHash(t *testing.T) {
	body := []byte("payload")
	got := Version(&Response{}, body)
	if want := mamori.VersionHash(body); got != want {
		t.Fatalf("Version = %q, want %q", got, want)
	}
}

func TestVersionHandlesNilResponse(t *testing.T) {
	body := []byte("payload")
	if got, want := Version(nil, body), mamori.VersionHash(body); got != want {
		t.Fatalf("Version(nil) = %q, want %q", got, want)
	}
}
