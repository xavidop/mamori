package https

import (
	"errors"
	"testing"

	"github.com/xavidop/mamori"
)

func TestNewRejectsNoEndpoints(t *testing.T) {
	if _, err := New(); err == nil {
		t.Fatal("New with no endpoints returned nil error")
	}
}

func TestNewRejectsUnnamedEndpoint(t *testing.T) {
	_, err := New(Endpoint{BaseURL: "https://api.test"})
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestNewRejectsDuplicateNames(t *testing.T) {
	_, err := New(
		Endpoint{Name: "a", BaseURL: "https://one.test"},
		Endpoint{Name: "a", BaseURL: "https://two.test"},
	)
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestNewRejectsNameWithSlash(t *testing.T) {
	// The name is the ref authority, so a slash would make the ref ambiguous
	// with the path that follows it.
	_, err := New(Endpoint{Name: "a/b", BaseURL: "https://api.test"})
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

// TestNewRejectsUnparsableBaseURL covers New's url.Parse branch.
//
// url.Parse is permissive, so most malformed input slips through it and is
// caught later by httpcore.New's scheme and host checks instead. An invalid
// percent escape is one of the few things it genuinely rejects, which makes it
// the input that actually exercises this branch rather than a neighbouring one.
func TestNewRejectsUnparsableBaseURL(t *testing.T) {
	_, err := New(Endpoint{Name: "a", BaseURL: "https://%zz"})
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

// TestNewRejectsNonHTTPScheme pins that the scheme is checked against a closed
// set, not merely tested for http://.
//
// An ftp:// typo or a ws:// paste satisfies both this provider's insecure-scheme
// gate and httpcore.New's scheme-and-host check, so without this the endpoint
// constructs cleanly and then fails on every resolve with net/http's
// "unsupported protocol scheme". That is the resolve-time failure New exists to
// prevent.
func TestNewRejectsNonHTTPScheme(t *testing.T) {
	for _, base := range []string{"ftp://api.test", "ws://api.test", "file:///etc/config"} {
		t.Run(base, func(t *testing.T) {
			_, err := New(Endpoint{Name: "a", BaseURL: base})
			if !errors.Is(err, mamori.ErrInvalid) {
				t.Fatalf("New(%q) err = %v, want ErrInvalid", base, err)
			}
		})
	}
}

// TestAllowInsecureDoesNotRescueOtherSchemes pins the scope of AllowInsecure: it
// permits cleartext http, and nothing else. Reading it as a general "skip the
// scheme check" switch would reopen exactly the hole TestNewRejectsNonHTTPScheme
// closes.
func TestAllowInsecureDoesNotRescueOtherSchemes(t *testing.T) {
	_, err := New(Endpoint{Name: "a", BaseURL: "ftp://api.test", AllowInsecure: true})
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid; AllowInsecure rescued a non-http scheme", err)
	}
}

// TestNewRejectsEmptyBaseURL pins that an omitted BaseURL fails at construction
// with this provider's own message, rather than reaching httpcore.New.
func TestNewRejectsEmptyBaseURL(t *testing.T) {
	_, err := New(Endpoint{Name: "a"})
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestNewRejectsInsecureBaseURL(t *testing.T) {
	_, err := New(Endpoint{Name: "a", BaseURL: "http://api.test"})
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestNewAllowsInsecureWhenOptedIn(t *testing.T) {
	if _, err := New(Endpoint{Name: "a", BaseURL: "http://api.test", AllowInsecure: true}); err != nil {
		t.Fatalf("New with AllowInsecure: %v", err)
	}
}

func TestScheme(t *testing.T) {
	p, err := New(Endpoint{Name: "a", BaseURL: "https://api.test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := p.Scheme(); got != "https" {
		t.Fatalf("Scheme = %q, want https", got)
	}
}

// TestProviderIsNotWatchable pins the deliberate absence of a native watch, so
// removing that decision cannot happen silently.
//
// This test only discriminates once Resolve exists, which Task 8 adds:
// WatchableProvider embeds Provider, so before Resolve is written the type
// cannot satisfy the interface no matter what else is added to it, and adding a
// Watch method alone would not fail this. Re-run the mutation after Task 8.
func TestProviderIsNotWatchable(t *testing.T) {
	p, err := New(Endpoint{Name: "a", BaseURL: "https://api.test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := any(p).(mamori.WatchableProvider); ok {
		t.Fatal("Provider implements WatchableProvider; a generic HTTP endpoint has no push channel, so mamori must poll it")
	}
}
