package https

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/xavidop/mamori"
)

// newTestProvider builds a Provider over f with the given endpoint options.
func newTestProvider(t *testing.T, f *fakeBackend, mutate func(*Endpoint)) *Provider {
	t.Helper()
	e := Endpoint{
		Name:    "billing",
		BaseURL: "https://api.test/v1",
		Client:  &http.Client{Transport: f.transport()},
	}
	if mutate != nil {
		mutate(&e)
	}
	p, err := New(e)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// mustRef parses a ref or fails the test.
func mustRef(t *testing.T, s string) mamori.Ref {
	t.Helper()
	r, err := mamori.ParseRef(s)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", s, err)
	}
	return r
}

func TestResolveReturnsBody(t *testing.T) {
	f := newFake()
	f.set("/v1/cfg", []byte(`{"region":"eu-west-1"}`))
	p := newTestProvider(t, f, nil)

	v, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != `{"region":"eu-west-1"}` {
		t.Fatalf("Bytes = %q", v.Bytes)
	}
	if v.Version == "" {
		t.Fatal("Version is empty; change detection needs one")
	}
}

func TestResolveSelectsWithPointer(t *testing.T) {
	f := newFake()
	f.set("/v1/cfg", []byte(`{"db":{"pass":"s3cr3t"}}`))
	p := newTestProvider(t, f, nil)

	v, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg#/db/pass"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "s3cr3t" {
		t.Fatalf("Bytes = %q, want s3cr3t", v.Bytes)
	}
}

func TestResolveSelectsTopLevelKey(t *testing.T) {
	f := newFake()
	f.set("/v1/cfg", []byte(`{"log.level":"debug"}`))
	p := newTestProvider(t, f, nil)

	v, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg#log.level"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "debug" {
		t.Fatalf("Bytes = %q, want debug", v.Bytes)
	}
}

func TestResolveUnknownEndpoint(t *testing.T) {
	f := newFake()
	p := newTestProvider(t, f, nil)

	_, err := p.Resolve(context.Background(), mustRef(t, "https://nope/cfg"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	// An unknown endpoint must NOT be ErrNotFound: that would silently apply the
	// field's default instead of reporting the misconfiguration.
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("unknown endpoint reported as ErrNotFound, which would hide it behind a default")
	}
}

func TestResolveMissingValueIsNotFound(t *testing.T) {
	f := newFake()
	p := newTestProvider(t, f, nil)

	_, err := p.Resolve(context.Background(), mustRef(t, "https://billing/absent"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestResolveMergesEndpointQueryAndHeader(t *testing.T) {
	f := newFake()
	f.set("/v1/cfg", []byte(`{}`))
	p := newTestProvider(t, f, func(e *Endpoint) {
		e.Query = url.Values{"env": {"prod"}}
		e.Header = http.Header{"X-Tenant": {"acme"}}
	})

	if _, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if f.lastQuery != "env=prod" {
		t.Fatalf("query = %q, want env=prod", f.lastQuery)
	}
	if got := f.lastHeader.Get("X-Tenant"); got != "acme" {
		t.Fatalf("X-Tenant = %q, want acme", got)
	}
}

func TestResolveMarksSensitive(t *testing.T) {
	f := newFake()
	f.set("/v1/cfg", []byte("s3cr3t"))
	p := newTestProvider(t, f, func(e *Endpoint) { e.Sensitive = true })

	v, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !v.Sensitive {
		t.Fatal("Sensitive = false, want true for a Sensitive endpoint")
	}
}

func TestResolveUsesConditionalGetOnSecondCall(t *testing.T) {
	f := newFake()
	f.set("/v1/cfg", []byte("payload"))
	p := newTestProvider(t, f, nil)

	first, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg"))
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	second, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg"))
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if f.lastHeader.Get("If-None-Match") == "" {
		t.Fatal("second Resolve sent no If-None-Match; the poll is not conditional")
	}
	if string(second.Bytes) != string(first.Bytes) {
		t.Fatalf("second Bytes = %q, want the cached %q", second.Bytes, first.Bytes)
	}
	if second.Version != first.Version {
		t.Fatalf("Version changed across an unmodified poll: %q then %q", first.Version, second.Version)
	}
}

// TestResolveRejectsDotSegments pins that a ref path cannot escape the path
// prefix its endpoint declares.
//
// This is reachable, not theoretical: expandRefVars substitutes ${VAR} from
// WithRefVars, whose values the application supplies at runtime, so a ref of
// https://billing/${TENANT}/cfg carries whatever TENANT holds. For an endpoint
// scoped to a tenant prefix, "../.." reaches another tenant's configuration
// without ever leaving the declared host, so the endpoint check that exists to
// contain exactly this never fires.
func TestResolveRejectsDotSegments(t *testing.T) {
	paths := []string{
		"../secrets", "a/../../b", "./cfg", "a/./b",
		// Backslash separators. Splitting on '/' alone leaves these as one
		// segment matching neither "." nor "..", so the check passes and the
		// request goes out with the backslashes percent encoded as %5C. IIS and
		// ASP.NET decode that and honour '\' as a directory separator, which is
		// the classic backslash traversal bypass, and BaseURL is operator
		// supplied with no platform restriction.
		`..\secrets`, `a\..\..\secrets`, `a/..\b`,
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			f := newFake()
			p := newTestProvider(t, f, nil)

			_, err := p.Resolve(context.Background(), mustRef(t, "https://billing/"+path))
			if !errors.Is(err, mamori.ErrInvalid) {
				t.Fatalf("Resolve(%q) err = %v, want ErrInvalid", path, err)
			}
			if errors.Is(err, mamori.ErrNotFound) {
				t.Fatalf("Resolve(%q) reported ErrNotFound, which would hide the traversal behind a field default", path)
			}
		})
	}
}

// TestResolveAllowsBackslashInAnOrdinaryKey pins the scope of the backslash
// rule: a backslash is treated as a separator when looking for dot segments,
// but a key that merely contains one is still an ordinary key. Rejecting every
// backslash outright would be simpler and would break a legitimate
// Windows-style key name on a generic HTTP backend.
func TestResolveAllowsBackslashInAnOrdinaryKey(t *testing.T) {
	f := newFake()
	f.set(`/v1/a\b`, []byte("payload"))
	p := newTestProvider(t, f, nil)

	v, err := p.Resolve(context.Background(), mustRef(t, `https://billing/a\b`))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "payload" {
		t.Fatalf("Bytes = %q, want payload", v.Bytes)
	}
}

// TestResolveRejectsEscapedDotSegments pins the other half: a percent-encoded
// traversal is refused exactly like a literal one, and no request goes out.
//
// This used to be a weaker guarantee. httpcore wrote a request path into
// url.URL.Path alone, which re-escapes the percent sign, so "%2e%2e" reached the
// backend as the harmless literal "%252e%252e" and needed no check of its own.
// httpcore now preserves a caller's escapes, because a backend whose key names
// contain slashes cannot be addressed without an encoded slash surviving to the
// wire, and an encoded traversal would survive with it. The dot-segment check
// therefore runs on the DECODED path, which catches both forms.
func TestResolveRejectsEscapedDotSegments(t *testing.T) {
	f := newFake()
	p := newTestProvider(t, f, nil)

	_, err := p.Resolve(context.Background(), mustRef(t, "https://billing/%2e%2e/secrets"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v reported ErrNotFound, which would hide the traversal behind a field default", err)
	}
	// The rejection happens before the round trip, so the backend never saw a
	// path at all, traversing or otherwise.
	if f.lastPath != "" {
		t.Fatalf("request reached the backend as %q; the rejection must precede the round trip", f.lastPath)
	}
}

// TestResolvePassesThroughUnknownOptions pins the DecodeOption conformance
// requirement: decoding is core's job, so the provider must not touch ?decode=.
func TestResolvePassesThroughUnknownOptions(t *testing.T) {
	f := newFake()
	f.set("/v1/cfg", []byte("cGF5bG9hZA=="))
	p := newTestProvider(t, f, nil)

	v, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg?decode=base64"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "cGF5bG9hZA==" {
		t.Fatalf("Bytes = %q; the provider decoded the value, which is core's job", v.Bytes)
	}
}

// TestNewCopiesQueryAndHeader pins that New defensively copies Endpoint.Query
// and Endpoint.Header.
//
// Storing them by reference makes a caller that keeps the map, to build several
// endpoints from one template, or simply because it did not expect New to
// retain it, able to change what goes on the wire long after construction. For
// Header that reaches credentials; for Query it reaches whatever the endpoint
// selects. This package already clones bytes in both directions in
// httpcore.Revalidator for the same reason.
//
// url.Values needs a DEEP copy: its values are slices, so a shallow map copy
// still shares the backing arrays and the mutation below would land anyway.
func TestNewCopiesQueryAndHeader(t *testing.T) {
	f := newFake()
	f.set("/v1/cfg", []byte("ok"))

	query := url.Values{"env": {"prod"}}
	header := http.Header{"X-Tenant": {"acme"}}
	p := newTestProvider(t, f, func(e *Endpoint) {
		e.Query = query
		e.Header = header
	})

	// Mutate the caller's maps every way that matters: replace a slice element
	// in place, extend a slice, and add a whole new key.
	query["env"][0] = "staging"
	query["extra"] = []string{"1"}
	header["X-Tenant"][0] = "evil"
	header.Set("X-Injected", "1")

	if _, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if f.lastQuery != "env=prod" {
		t.Fatalf("query = %q, want env=prod: New retained the caller's url.Values", f.lastQuery)
	}
	if got := f.lastHeader.Get("X-Tenant"); got != "acme" {
		t.Fatalf("X-Tenant = %q, want acme: New retained the caller's http.Header", got)
	}
	if got := f.lastHeader.Get("X-Injected"); got != "" {
		t.Fatalf("X-Injected = %q, want empty: New retained the caller's http.Header", got)
	}
}
